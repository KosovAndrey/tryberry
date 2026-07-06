package postgres

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// WinbackRepo — состояние win-back-цепочки конца триала (trial_winbacks):
// персональный код и отметки трёх стадий пушей. Логика стадий — в notifier
// (cmd/notifier/winback.go), здесь только выборки/отметки.
type WinbackRepo struct {
	db *pgxpool.Pool
}

func NewWinbackRepo(db *pgxpool.Pool) *WinbackRepo {
	return &WinbackRepo{db: db}
}

// WinbackCandidate — триальщик, которому пора завести win-back (стадия 1).
type WinbackCandidate struct {
	UserID        int64
	PlanExpiresAt time.Time
}

// WinbackRow — строка цепочки для отправки пуша.
type WinbackRow struct {
	UserID        int64
	Code          string
	CodeExpiresAt time.Time
}

// ListStage1New — триальщики, входящие в окно стадии 1 (триал истекает до
// until), у которых ещё нет win-back-строки. Синтетиков не трогаем.
func (r *WinbackRepo) ListStage1New(ctx context.Context, until time.Time) ([]WinbackCandidate, error) {
	const q = `
		SELECT u.id, u.plan_expires_at
		FROM users u
		WHERE u.plan = 'trial' AND NOT u.is_synthetic
		  AND u.plan_expires_at > NOW() AND u.plan_expires_at <= $1
		  AND NOT EXISTS (SELECT 1 FROM trial_winbacks w WHERE w.user_id = u.id)`
	rows, err := r.db.Query(ctx, q, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WinbackCandidate
	for rows.Next() {
		var c WinbackCandidate
		if err := rows.Scan(&c.UserID, &c.PlanExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// Create — завести win-back-строку (код уже создан в promo_codes).
func (r *WinbackRepo) Create(ctx context.Context, userID, promoCodeID int64, code string, codeExpiresAt time.Time) error {
	const q = `
		INSERT INTO trial_winbacks (user_id, promo_code_id, code, code_expires_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO NOTHING`
	_, err := r.db.Exec(ctx, q, userID, promoCodeID, code, codeExpiresAt)
	return err
}

// ListStage1Unsent — строки с неотправленной стадией 1 (вкл. ретраи после
// сбоя отправки прошлым тиком).
func (r *WinbackRepo) ListStage1Unsent(ctx context.Context) ([]WinbackRow, error) {
	const q = `
		SELECT w.user_id, w.code, w.code_expires_at
		FROM trial_winbacks w
		WHERE w.stage1_sent_at IS NULL AND w.code_expires_at > NOW()`
	return r.scanRows(ctx, q)
}

// ListStage2Due — стадия 2: триал истёк, код ещё жив, юзер не купил
// (plan всё ещё 'trial', действующий план уже free).
func (r *WinbackRepo) ListStage2Due(ctx context.Context) ([]WinbackRow, error) {
	const q = `
		SELECT w.user_id, w.code, w.code_expires_at
		FROM trial_winbacks w
		JOIN users u ON u.id = w.user_id
		WHERE w.stage1_sent_at IS NOT NULL AND w.stage2_sent_at IS NULL
		  AND w.code_expires_at > NOW()
		  AND u.plan = 'trial' AND u.plan_expires_at <= NOW()`
	return r.scanRows(ctx, q)
}

// ListStage3Due — стадия 3 (последний звонок): до сгорания кода меньше lead,
// код не погашен, юзер так и не купил.
func (r *WinbackRepo) ListStage3Due(ctx context.Context, lead time.Duration) ([]WinbackRow, error) {
	const q = `
		SELECT w.user_id, w.code, w.code_expires_at
		FROM trial_winbacks w
		JOIN users u ON u.id = w.user_id
		WHERE w.stage2_sent_at IS NOT NULL AND w.stage3_sent_at IS NULL
		  AND w.code_expires_at > NOW() AND w.code_expires_at <= NOW() + $1
		  AND u.plan = 'trial' AND u.plan_expires_at <= NOW()
		  AND NOT EXISTS (
			SELECT 1 FROM promo_redemptions pr
			WHERE pr.code_id = w.promo_code_id AND pr.user_id = w.user_id)`
	return r.scanRows(ctx, q, lead)
}

// MarkStage — отметить стадию отправленной.
func (r *WinbackRepo) MarkStage(ctx context.Context, userID int64, stage int) error {
	var col string
	switch stage {
	case 1:
		col = "stage1_sent_at"
	case 2:
		col = "stage2_sent_at"
	case 3:
		col = "stage3_sent_at"
	default:
		return fmt.Errorf("winback: unknown stage %d", stage)
	}
	_, err := r.db.Exec(ctx,
		`UPDATE trial_winbacks SET `+col+` = NOW() WHERE user_id = $1`, userID)
	return err
}

// HasWinback — у кого из userIDs есть win-back-строка: таким reconciler не шлёт
// общий paused-notice (его роль играет стадия 2 цепочки).
func (r *WinbackRepo) HasWinback(ctx context.Context, userIDs []int64) (map[int64]struct{}, error) {
	if len(userIDs) == 0 {
		return nil, nil
	}
	rows, err := r.db.Query(ctx,
		`SELECT user_id FROM trial_winbacks WHERE user_id = ANY($1)`, userIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make(map[int64]struct{})
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = struct{}{}
	}
	return out, rows.Err()
}

func (r *WinbackRepo) scanRows(ctx context.Context, q string, args ...any) ([]WinbackRow, error) {
	rows, err := r.db.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []WinbackRow
	for rows.Next() {
		var w WinbackRow
		if err := rows.Scan(&w.UserID, &w.Code, &w.CodeExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}
