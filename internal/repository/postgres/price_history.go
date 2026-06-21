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

// Stats — агрегаты цены по товару для «честной цены». Рассчитан на CHANGE-ONLY
// хранение (в price_history пишем только смену цены, см. cmd/scraper): каждая запись —
// начало сегмента, действующего до следующей записи (последний — до now). Поэтому:
//   - min за окно берём по сегментам, ПЕРЕСЕКАЮЩИМ окно (включая «якорь» — сегмент,
//     активный на границе окна, даже если его запись старше окна);
//   - медиана за 30д ВЗВЕШЕНА ПО ДЛИТЕЛЬНОСТИ (lower weighted median): «обычная цена» —
//     та, где товар провёл половину времени, а не просто середина по числу записей
//     (иначе редкая краткая акция перекосила бы вердикт).
//
// Опирается на индекс (product_id, recorded_at).
func (r *PriceHistoryRepo) Stats(ctx context.Context, productID int64, now time.Time) (domain.PriceStats, error) {
	const q = `
		WITH seg AS (
			SELECT price, recorded_at AS t0,
			       lead(recorded_at, 1, $2::timestamptz) OVER (ORDER BY recorded_at) AS t1
			FROM price_history
			WHERE product_id = $1
		),
		seg30 AS (
			SELECT price,
			       GREATEST(t0, $2::timestamptz - interval '30 days') AS s,
			       LEAST(t1, $2::timestamptz)                         AS e
			FROM seg
			WHERE t1 > $2::timestamptz - interval '30 days' AND t0 < $2::timestamptz
		),
		dur30 AS (
			SELECT price, EXTRACT(EPOCH FROM (e - s)) AS d FROM seg30 WHERE e > s
		),
		wmed AS (
			SELECT price,
			       SUM(d) OVER (ORDER BY price) AS cum,
			       SUM(d) OVER ()               AS tot
			FROM dur30
		)
		SELECT
			(SELECT min(price) FROM dur30),
			(SELECT price FROM wmed WHERE tot > 0 AND cum >= tot / 2.0 ORDER BY price LIMIT 1),
			(SELECT min(price) FROM seg WHERE t1 > $2::timestamptz - interval '90 days'),
			(SELECT min(price) FROM price_history WHERE product_id = $1),
			(SELECT count(*) FROM dur30),
			(SELECT count(*) FROM price_history WHERE product_id = $1),
			(SELECT min(recorded_at) FROM price_history WHERE product_id = $1)`

	var (
		min30, median30, min90, minAll *float64
		seg30, countAll                int64
		since                          *time.Time
	)
	err := r.db.QueryRow(ctx, q, productID, now).
		Scan(&min30, &median30, &min90, &minAll, &seg30, &countAll, &since)
	if err != nil {
		return domain.PriceStats{}, err
	}

	s := domain.PriceStats{Seg30: int(seg30), CountAll: int(countAll), HasData: countAll > 0}
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
