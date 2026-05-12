package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type PriceHistoryRepo struct {
	db *pgxpool.Pool
}

func NewPriceHistoryRepo(db *pgxpool.Pool) *PriceHistoryRepo {
	return &PriceHistoryRepo{db: db}
}

func (r *PriceHistoryRepo) Insert(ctx context.Context, productID int64, price float64) error {
	const q = `
		INSERT INTO price_history (product_id, price)
		VALUES ($1, $2)`

	_, err := r.db.Exec(ctx, q, productID, price)
	return err
}

// GetLatest — последняя записанная цена. Используется как fallback если Redis недоступен.
func (r *PriceHistoryRepo) GetLatest(ctx context.Context, productID int64) (float64, time.Time, error) {
	const q = `
		SELECT price, recorded_at
		FROM price_history
		WHERE product_id = $1
		ORDER BY recorded_at DESC
		LIMIT 1`

	var price float64
	var recordedAt time.Time
	err := r.db.QueryRow(ctx, q, productID).Scan(&price, &recordedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, domain.ErrNotFound
	}
	if err != nil {
		return 0, time.Time{}, err
	}
	return price, recordedAt, nil
}
