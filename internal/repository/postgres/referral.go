package postgres

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type ReferralRepo struct {
	db *pgxpool.Pool
}

func NewReferralRepo(db *pgxpool.Pool) *ReferralRepo {
	return &ReferralRepo{db: db}
}

// SetReferrer — атрибуция «кто привёл». Срабатывает только если:
// аккаунт свежий (окно атрибуции), реферер ещё не записан и это не сам юзер.
// Возвращает true, если атрибуция засчитана.
func (r *ReferralRepo) SetReferrer(ctx context.Context, refereeTgID, referrerTgID int64, window time.Duration) (bool, error) {
	const q = `
		UPDATE users u
		SET referred_by = ref.id
		FROM users ref
		WHERE u.telegram_id = $1
		  AND ref.telegram_id = $2
		  AND u.referred_by IS NULL
		  AND u.id <> ref.id
		  AND u.created_at > NOW() - $3::interval`
	tag, err := r.db.Exec(ctx, q, refereeTgID, referrerTgID, window)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// PendingActivation — кандидат на награду за «активного» друга.
type PendingActivation struct {
	ReferrerID    int64
	ReferrerTgID  int64
	ReferrerPlan  string
	ReferrerExpAt *time.Time
	RefereeID     int64
	RefereeName   string // username друга (для уведомления, может быть пустым)
}

// ListPendingActivations — приглашённые, прожившие minAge с активной подпиской
// (товарной или поисковой), за которых награда 'activated' ещё не начислена.
func (r *ReferralRepo) ListPendingActivations(ctx context.Context, minAge time.Duration, limit int) ([]PendingActivation, error) {
	const q = `
		SELECT ref.id, ref.telegram_id, ref.plan, ref.plan_expires_at,
		       u.id, COALESCE(u.username, '')
		FROM users u
		JOIN users ref ON ref.id = u.referred_by
		WHERE u.referred_by IS NOT NULL
		  AND u.created_at <= NOW() - $1::interval
		  AND NOT EXISTS (
		      SELECT 1 FROM referral_rewards rr
		      WHERE rr.referee_id = u.id AND rr.event = 'activated')
		  AND (EXISTS (SELECT 1 FROM subscriptions s WHERE s.user_id = u.id AND s.active)
		    OR EXISTS (SELECT 1 FROM search_subscriptions ss WHERE ss.user_id = u.id AND ss.active))
		LIMIT $2`
	rows, err := r.db.Query(ctx, q, minAge, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PendingActivation
	for rows.Next() {
		var p PendingActivation
		if err := rows.Scan(&p.ReferrerID, &p.ReferrerTgID, &p.ReferrerPlan, &p.ReferrerExpAt,
			&p.RefereeID, &p.RefereeName); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GrantReward атомарно начисляет награду рефереру: запись в аудит (UNIQUE по
// (referee, event) отсекает повтор — тогда false) + потолок за скользящий год +
// опциональная выдача/продление плана (setPlan=false — только аудит).
func (r *ReferralRepo) GrantReward(
	ctx context.Context,
	referrerID, refereeID int64,
	event string, days int, capDays int,
	setPlan bool, plan string, expiresAt time.Time,
) (bool, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Потолок: сумма начислений за год, под блокировкой вставки ниже.
	var granted int
	if err := tx.QueryRow(ctx,
		`SELECT COALESCE(SUM(days), 0) FROM referral_rewards
		 WHERE referrer_id = $1 AND created_at > NOW() - INTERVAL '365 days'`,
		referrerID).Scan(&granted); err != nil {
		return false, err
	}
	if granted+days > capDays {
		return false, nil // потолок — просто не начисляем (и не помечаем событие)
	}

	tag, err := tx.Exec(ctx,
		`INSERT INTO referral_rewards (referrer_id, referee_id, event, days)
		 VALUES ($1, $2, $3, $4)
		 ON CONFLICT (referee_id, event) DO NOTHING`,
		referrerID, refereeID, event, days)
	if err != nil {
		return false, err
	}
	if tag.RowsAffected() == 0 {
		return false, nil // уже начисляли
	}

	if setPlan {
		if _, err := tx.Exec(ctx,
			`UPDATE users SET plan = $2, plan_expires_at = $3, plan_reminded_at = NULL
			 WHERE id = $1`, referrerID, plan, expiresAt); err != nil {
			return false, err
		}
	}

	return true, tx.Commit(ctx)
}

// Stats — статистика для /ref: сколько привёл, сколько активировалось,
// сколько дней начислено суммарно.
type ReferralStats struct {
	Invited     int
	Activated   int
	DaysGranted int
}

func (r *ReferralRepo) Stats(ctx context.Context, userID int64) (ReferralStats, error) {
	const q = `
		SELECT
		    (SELECT count(*) FROM users WHERE referred_by = $1),
		    (SELECT count(*) FROM referral_rewards WHERE referrer_id = $1 AND event = 'activated'),
		    (SELECT COALESCE(SUM(days), 0) FROM referral_rewards WHERE referrer_id = $1)`
	var s ReferralStats
	err := r.db.QueryRow(ctx, q, userID).Scan(&s.Invited, &s.Activated, &s.DaysGranted)
	return s, err
}
