package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type ProductRepo struct {
	db *pgxpool.Pool
}

func NewProductRepo(db *pgxpool.Pool) *ProductRepo {
	return &ProductRepo{db: db}
}

func (r *ProductRepo) Upsert(ctx context.Context, url, name, imageURL, marketplace string) (*domain.Product, error) {
	const q = `
		INSERT INTO products (url, name, image_url, marketplace)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (url) DO UPDATE
			SET name        = EXCLUDED.name,
			    image_url   = EXCLUDED.image_url,
			    marketplace = EXCLUDED.marketplace,
			    updated_at  = NOW()
		RETURNING id, url, name, image_url, marketplace, created_at, updated_at`

	p := &domain.Product{}
	err := withSpan(ctx, "upsert_product", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, url, name, imageURL, marketplace).
			Scan(&p.ID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &p.CreatedAt, &p.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductRepo) GetByID(ctx context.Context, id int64) (*domain.Product, error) {
	const q = `
		SELECT id, url, name, image_url, marketplace, created_at, updated_at
		FROM products WHERE id = $1`

	p := &domain.Product{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&p.ID, &p.URL, &p.Name, &p.ImageURL, &p.Marketplace, &p.CreatedAt, &p.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return p, nil
}

func (r *ProductRepo) UpdateScrapedData(ctx context.Context, id int64, name, imageURL string) error {
	const q = `
		UPDATE products
		SET name = $2, image_url = $3, updated_at = NOW()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, q, id, name, imageURL)
	return err
}

func (r *ProductRepo) GetActiveProductIDs(ctx context.Context) ([]int64, error) {
	const q = `
		SELECT DISTINCT product_id
		FROM subscriptions
		WHERE active = TRUE`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var ids []int64
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}
