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

// PricePoint — одна точка серии для графика: момент смены цены + цена сегмента,
// действующая до следующей точки (последняя — до now, дорисовывает клиент).
type PricePoint struct {
	RecordedAt time.Time
	Price      float64
}

// seriesMaxPoints — потолок числа точек, отдаваемых на график. На change-only
// хранении почти недостижим; защищает память/трафик от аномального товара.
const seriesMaxPoints = 5000

// Series — точки истории цены товара в окне [from, to] ДЛЯ ступенчатого графика.
// Из-за change-only хранения добавляем «якорь» — последнюю запись со временем
// строго до from (сегмент, активный на левой границе окна), смещая её время к
// from, чтобы линия начиналась ровно от края, а не повисала. Опирается на индекс
// (product_id, recorded_at); партиции прунятся по recorded_at.
func (r *PriceHistoryRepo) Series(ctx context.Context, productID int64, from, to time.Time) ([]PricePoint, error) {
	const q = `
		(
			SELECT $3::timestamptz AS recorded_at, price
			FROM price_history
			WHERE product_id = $1 AND recorded_at < $3::timestamptz
			ORDER BY recorded_at DESC
			LIMIT 1
		)
		UNION ALL
		(
			SELECT recorded_at, price
			FROM price_history
			WHERE product_id = $1
			  AND recorded_at >= $3::timestamptz
			  AND recorded_at <= $4::timestamptz
			ORDER BY recorded_at
			LIMIT $2
		)
		ORDER BY recorded_at`

	rows, err := r.db.Query(ctx, q, productID, seriesMaxPoints, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PricePoint
	for rows.Next() {
		var p PricePoint
		if err := rows.Scan(&p.RecordedAt, &p.Price); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// EarliestRecordedAt — самая ранняя записанная точка товара (для бэкфилла:
// дотягиваем из маркетплейс-истории только то, что СТАРШЕ). ok=false — истории нет.
func (r *PriceHistoryRepo) EarliestRecordedAt(ctx context.Context, productID int64) (time.Time, bool, error) {
	var t *time.Time
	err := r.db.QueryRow(ctx,
		`SELECT min(recorded_at) FROM price_history WHERE product_id = $1`, productID).Scan(&t)
	if err != nil {
		return time.Time{}, false, err
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return *t, true, nil
}

// PrependOlder дозаливает исторические точки (из WB price-history.json и т.п.),
// которые СТАРШЕ самой ранней уже записанной точки товара (или все, если истории
// нет). Покрывает и новые товары, и добавленные до появления бэкфилла. Идемпотентно
// и самоограничивающе: после первого прохода наш минимум = старейшая точка
// маркетплейса, повторные вызовы ничего не вставляют. Возвращает число вставленных.
//
// Гонко-безопасно: транзакция + xact-advisory-lock по product_id; внутри ПОВТОРНО
// читаем минимум (под локом) и фильтруем — параллельный скрейп не задвоит.
// ВНИМАНИЕ: партиции под прошлые месяцы должны существовать (вызывающий заранее
// делает partition.Manager.EnsureForTimes) — иначе INSERT упадёт.
func (r *PriceHistoryRepo) PrependOlder(ctx context.Context, productID int64, points []PricePoint) (int, error) {
	if len(points) == 0 {
		return 0, nil
	}

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `SELECT pg_advisory_xact_lock($1)`, productID); err != nil {
		return 0, err
	}

	var earliest *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT min(recorded_at) FROM price_history WHERE product_id = $1`, productID).Scan(&earliest); err != nil {
		return 0, err
	}

	b := &pgx.Batch{}
	n := 0
	for _, p := range points {
		if earliest != nil && !p.RecordedAt.Before(*earliest) {
			continue // не старше нашей истории — пропускаем (есть своя точка)
		}
		b.Queue(
			`INSERT INTO price_history (product_id, price, recorded_at) VALUES ($1, $2, $3)`,
			productID, p.Price, p.RecordedAt)
		n++
	}
	if n == 0 {
		return 0, nil
	}

	br := tx.SendBatch(ctx, b)
	for i := 0; i < n; i++ {
		if _, err := br.Exec(); err != nil {
			br.Close()
			return 0, err
		}
	}
	if err := br.Close(); err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return n, nil
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
