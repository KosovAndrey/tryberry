package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type SearchQueryRepo struct {
	db *pgxpool.Pool
}

func NewSearchQueryRepo(db *pgxpool.Pool) *SearchQueryRepo {
	return &SearchQueryRepo{db: db}
}

// Upsert — создать поисковый запрос по normalized_url или вернуть существующий.
// normalized_url — ключ дедупликации между пользователями (UNIQUE).
// Возвращает (query, created, error): created=true если запрос создан впервые.
//
// last_scraped_at НЕ трогаем при конфликте — он принадлежит планировщику,
// а не пользователю, который пере-подписался на тот же запрос.
func (r *SearchQueryRepo) Upsert(
	ctx context.Context,
	marketplace, normalizedURL, queryText string,
	filters []byte,
) (*domain.SearchQuery, bool, error) {
	if filters == nil {
		filters = []byte("{}")
	}

	const q = `
		INSERT INTO search_queries (marketplace, normalized_url, query_text, filters)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (normalized_url) DO UPDATE
			SET query_text = EXCLUDED.query_text,
			    filters    = EXCLUDED.filters
		RETURNING id, marketplace, normalized_url, query_text, filters,
		          last_scraped_at, created_at,
		          (xmax = 0) AS inserted`

	sq := &domain.SearchQuery{}
	var inserted bool
	err := withSpan(ctx, "upsert_search_query", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, marketplace, normalizedURL, queryText, filters).
			Scan(&sq.ID, &sq.Marketplace, &sq.NormalizedURL, &sq.QueryText,
				&sq.Filters, &sq.LastScrapedAt, &sq.CreatedAt, &inserted)
	})
	if err != nil {
		return nil, false, err
	}
	return sq, inserted, nil
}

func (r *SearchQueryRepo) GetByID(ctx context.Context, id int64) (*domain.SearchQuery, error) {
	const q = `
		SELECT id, marketplace, normalized_url, query_text, filters,
		       last_scraped_at, created_at
		FROM search_queries WHERE id = $1`

	sq := &domain.SearchQuery{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&sq.ID, &sq.Marketplace, &sq.NormalizedURL, &sq.QueryText,
			&sq.Filters, &sq.LastScrapedAt, &sq.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return sq, nil
}

// GetScrapable — поисковые запросы, на которые есть хотя бы одна активная подписка.
// Сортировка по last_scraped_at NULLS FIRST: давно (или ни разу) не скрейпленные —
// первыми. Планировщик идёт по этому списку, уважая паузы и rate-limit WB.
func (r *SearchQueryRepo) GetScrapable(ctx context.Context) ([]*domain.SearchQuery, error) {
	const q = `
		SELECT sq.id, sq.marketplace, sq.normalized_url, sq.query_text, sq.filters,
		       sq.last_scraped_at, sq.created_at
		FROM search_queries sq
		WHERE EXISTS (
			SELECT 1 FROM search_subscriptions ss
			WHERE ss.search_query_id = sq.id AND ss.active = TRUE
		)
		ORDER BY sq.last_scraped_at ASC NULLS FIRST`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []*domain.SearchQuery
	for rows.Next() {
		sq := &domain.SearchQuery{}
		if err := rows.Scan(&sq.ID, &sq.Marketplace, &sq.NormalizedURL, &sq.QueryText,
			&sq.Filters, &sq.LastScrapedAt, &sq.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, sq)
	}
	return out, rows.Err()
}

// UpdateLastScraped — отметить время последнего скрейпинга выдачи.
func (r *SearchQueryRepo) UpdateLastScraped(ctx context.Context, id int64) error {
	const q = `UPDATE search_queries SET last_scraped_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id)
	return err
}
