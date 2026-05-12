package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type NotificationRepo struct {
	db *pgxpool.Pool
}

func NewNotificationRepo(db *pgxpool.Pool) *NotificationRepo {
	return &NotificationRepo{db: db}
}

func (r *NotificationRepo) Insert(ctx context.Context, n *domain.Notification) error {
	const q = `
		INSERT INTO notifications (subscription_id, old_price, new_price, idempotency_key)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (idempotency_key) DO NOTHING`

	_, err := r.db.Exec(ctx, q, n.SubscriptionID, n.OldPrice, n.NewPrice, n.IdempotencyKey)
	return err
}

func (r *NotificationRepo) ExistsByKey(ctx context.Context, key string) (bool, error) {
	const q = `SELECT EXISTS(SELECT 1 FROM notifications WHERE idempotency_key = $1)`

	var exists bool
	err := r.db.QueryRow(ctx, q, key).Scan(&exists)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	return exists, err
}
