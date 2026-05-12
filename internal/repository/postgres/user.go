package postgres

import (
	"context"
	"errors"

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
		RETURNING id, telegram_id, username, created_at`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, telegramID, username).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return u, nil
}

func (r *UserRepo) GetByTelegramID(ctx context.Context, telegramID int64) (*domain.User, error) {
	const q = `
		SELECT id, telegram_id, username, created_at
		FROM users WHERE telegram_id = $1`

	u := &domain.User{}
	err := r.db.QueryRow(ctx, q, telegramID).
		Scan(&u.ID, &u.TelegramID, &u.Username, &u.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return u, nil
}
