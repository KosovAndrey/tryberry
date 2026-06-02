package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type UserRepo struct {
	db *pgxpool.Pool
}

func NewUserRepo(db *pgxpool.Pool) *UserRepo {
	return &UserRepo{db: db}
}

func (r *UserRepo) Upsert(ctx context.Context, telegramID int64, username string) (*domain.User, error) {
	const q = `
		INSERT INTO users (telegram_id, username)
		VALUES ($1, $2)
		ON CONFLICT (telegram_id) DO UPDATE
			SET username = EXCLUDED.username
		RETURNING id, telegram_id, username, created_at, plan, plan_expires_at, trial_used`

	u := &domain.User{}
	err := withSpan(ctx, "upsert_user", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, telegramID, username).
			Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed)
	})
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) GetByTelegramID(ctx context.Context, telegramID int64) (*domain.User, error) {
	const q = `
		SELECT id, telegram_id, username, created_at, plan, plan_expires_at, trial_used
		FROM users WHERE telegram_id = $1`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, telegramID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt, &u.Plan, &u.PlanExpiresAt, &u.TrialUsed)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}

// SetPlan — выставить план и срок (expiresAt=nil → бессрочно). Для /grant и /revoke.
func (r *UserRepo) SetPlan(ctx context.Context, telegramID int64, plan string, expiresAt *time.Time) error {
	const q = `UPDATE users SET plan = $2, plan_expires_at = $3 WHERE telegram_id = $1`
	tag, err := r.db.Exec(ctx, q, telegramID, plan, expiresAt)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ActivateTrial — однократно включить триал. Возвращает false, если триал уже
// использовался (или пользователь не найден).
func (r *UserRepo) ActivateTrial(ctx context.Context, telegramID int64, expiresAt time.Time) (bool, error) {
	const q = `
		UPDATE users
		SET plan = 'trial', plan_expires_at = $2, trial_used = TRUE
		WHERE telegram_id = $1 AND trial_used = FALSE`
	tag, err := r.db.Exec(ctx, q, telegramID, expiresAt)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// UserUsage — строка для админского /users: план + занятость лимитов.
type UserUsage struct {
	TelegramID    int64
	Username      string
	Plan          string
	PlanExpiresAt *time.Time
	TrialUsed     bool
	Products      int
	Searches      int
}

// ListWithUsage — пользователи с числом активных подписок, по убыванию активности.
func (r *UserRepo) ListWithUsage(ctx context.Context, limit int) ([]UserUsage, error) {
	const q = `
		SELECT u.telegram_id, COALESCE(u.username, ''), u.plan, u.plan_expires_at, u.trial_used,
		       (SELECT count(*) FROM subscriptions s WHERE s.user_id = u.id AND s.active) AS products,
		       (SELECT count(*) FROM search_subscriptions ss WHERE ss.user_id = u.id AND ss.active) AS searches
		FROM users u
		ORDER BY products DESC, searches DESC, u.created_at DESC
		LIMIT $1`
	rows, err := r.db.Query(ctx, q, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UserUsage
	for rows.Next() {
		var x UserUsage
		if err := rows.Scan(&x.TelegramID, &x.Username, &x.Plan, &x.PlanExpiresAt, &x.TrialUsed, &x.Products, &x.Searches); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
