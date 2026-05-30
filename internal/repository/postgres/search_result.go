package postgres

import (
	"context"

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
