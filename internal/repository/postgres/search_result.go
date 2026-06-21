package postgres

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type SearchResultRepo struct {
	db *pgxpool.Pool
}

func NewSearchResultRepo(db *pgxpool.Pool) *SearchResultRepo {
	return &SearchResultRepo{db: db}
}

// Upsert — записать/обновить позицию товара в выдаче запроса.
// При каждом скрейпе вызывается для каждого товара.
//
// Возвращает isNew=true если товар появился в выдаче впервые — это сигнал
// движку: нужно завести baseline в search_subscription_products для всех
// активных подписок на этот запрос (см. §4).
//
// last_seen_at обновляется только для присутствующих товаров; исчезнувшие
// «отстают» по last_seen_at и могут быть забыты по TTL отдельной чисткой.
func (r *SearchResultRepo) Upsert(ctx context.Context, queryID, productID int64, position int, price float64) (bool, error) {
	const q = `
		INSERT INTO search_results (search_query_id, product_id, position, last_price)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (search_query_id, product_id) DO UPDATE
			SET position     = EXCLUDED.position,
			    last_price   = EXCLUDED.last_price,
			    last_seen_at = NOW()
		RETURNING (xmax = 0) AS inserted`

	var isNew bool
	err := withSpan(ctx, "upsert_search_result", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, queryID, productID, position, price).Scan(&isNew)
	})
	if err != nil {
		return false, err
	}
	return isNew, nil
}

// ResultUpsert — одна строка выдачи для батч-апсерта.
type ResultUpsert struct {
	ProductID int64
	Position  int
	Price     float64
}

// UpsertBatch апсертит всю выдачу запроса за ОДИН round-trip (pgx.Batch). Вместо N
// отдельных Upsert на скрейп (сотни item'ов, особенно reseller 1-мин).
//
// NO-OP SUPPRESSION + HEARTBEAT: строку перезаписываем только если цена/позиция
// изменились ЛИБО last_seen_at устарел (>1ч). Так режем MVCC-чёрн (на стабильной
// выдаче почти ничего не пишется), но liveness сохраняем — last_seen_at у
// присутствующих товаров обновляется минимум раз в час (для будущей TTL-чистки
// выпавших товаров). isNew не возвращаем — в searchloop он не используется.
func (r *SearchResultRepo) UpsertBatch(ctx context.Context, queryID int64, rows []ResultUpsert) error {
	if len(rows) == 0 {
		return nil
	}
	const q = `
		INSERT INTO search_results (search_query_id, product_id, position, last_price)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (search_query_id, product_id) DO UPDATE
			SET position     = EXCLUDED.position,
			    last_price   = EXCLUDED.last_price,
			    last_seen_at = NOW()
		WHERE search_results.last_price   IS DISTINCT FROM EXCLUDED.last_price
		   OR search_results.position     IS DISTINCT FROM EXCLUDED.position
		   OR search_results.last_seen_at <  NOW() - INTERVAL '1 hour'`

	b := &pgx.Batch{}
	for _, row := range rows {
		b.Queue(q, queryID, row.ProductID, row.Position, row.Price)
	}
	br := r.db.SendBatch(ctx, b)
	defer br.Close()
	for range rows {
		if _, err := br.Exec(); err != nil {
			return err
		}
	}
	return nil
}

// GetByQueryID — текущая выдача запроса, по позиции.
func (r *SearchResultRepo) GetByQueryID(ctx context.Context, queryID int64) ([]*domain.SearchResultItem, error) {
	const q = `
		SELECT search_query_id, product_id, position, last_price, first_seen_at, last_seen_at
		FROM search_results
		WHERE search_query_id = $1
		ORDER BY position`

	rows, err := r.db.Query(ctx, q, queryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.SearchResultItem
	for rows.Next() {
		it := &domain.SearchResultItem{}
		if err := rows.Scan(&it.SearchQueryID, &it.ProductID, &it.Position,
			&it.LastPrice, &it.FirstSeenAt, &it.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, it)
	}
	return out, rows.Err()
}
