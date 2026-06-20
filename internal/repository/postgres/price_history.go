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

// Stats — агрегаты цены по товару одним проходом: минимум/медиана за 30 дней,
// минимум за 90 дней и за всё наблюдение, число точек и дата первой записи. Опирается
// на индекс (product_id, recorded_at). Для фичи «честная цена» (анти-фейк-скидка).
func (r *PriceHistoryRepo) Stats(ctx context.Context, productID int64, now time.Time) (domain.PriceStats, error) {
	const q = `
		SELECT
			min(price) FILTER (WHERE recorded_at >= $2::timestamptz - interval '30 days'),
			percentile_cont(0.5) WITHIN GROUP (ORDER BY price)
				FILTER (WHERE recorded_at >= $2::timestamptz - interval '30 days'),
			min(price) FILTER (WHERE recorded_at >= $2::timestamptz - interval '90 days'),
			min(price),
			count(*) FILTER (WHERE recorded_at >= $2::timestamptz - interval '30 days'),
			count(*),
			min(recorded_at)
		FROM price_history
		WHERE product_id = $1`

	var (
		min30, median30, min90, minAll *float64
		count30, countAll              int64
		since                          *time.Time
	)
	err := r.db.QueryRow(ctx, q, productID, now).
		Scan(&min30, &median30, &min90, &minAll, &count30, &countAll, &since)
	if err != nil {
		return domain.PriceStats{}, err
	}

	s := domain.PriceStats{Count30: int(count30), CountAll: int(countAll), HasData: countAll > 0}
	if min30 != nil {
		s.Min30 = *min30
	}
	if median30 != nil {
		s.Median30 = *median30
	}
	if min90 != nil {
		s.Min90 = *min90
	}
	if minAll != nil {
		s.MinAll = *minAll
	}
	if since != nil {
		s.Since = *since
	}
	return s, nil
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
