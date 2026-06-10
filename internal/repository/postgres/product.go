package postgres

import (
	"context"
	"errors"
	"time"

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

// SchedulableProduct — строка планирования: товар + один его активный подписчик
// (с тарифом владельца). Планировщик группирует по ProductID и считает
// минимальный эффективный интервал среди подписчиков (источник истины —
// domain.Plans), решая, пора ли скрейпить. Зеркало SearchQueryRepo.GetSchedulable.
type SchedulableProduct struct {
	ProductID      int64
	URL            string
	LastEnqueuedAt *time.Time
	OwnerPlan      string
	PlanExpiresAt  *time.Time
}

// GetSchedulableProducts — по строке на каждую активную товарную подписку: товар
// + план владельца. MIN-интервал и решение «пора» планировщик считает в Go.
func (r *ProductRepo) GetSchedulableProducts(ctx context.Context) ([]SchedulableProduct, error) {
	const q = `
		SELECT p.id, p.url, p.last_enqueued_at, u.plan, u.plan_expires_at
		FROM products p
		JOIN subscriptions s ON s.product_id = p.id AND s.active = TRUE
		JOIN users u ON u.id = s.user_id`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []SchedulableProduct
	for rows.Next() {
		var p SchedulableProduct
		if err := rows.Scan(&p.ProductID, &p.URL, &p.LastEnqueuedAt, &p.OwnerPlan, &p.PlanExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ClaimEnqueued — отметить товары поставленными в очередь (last_enqueued_at=NOW).
// Планировщик-синглтон делает это перед эмиссией в Kafka.
func (r *ProductRepo) ClaimEnqueued(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `UPDATE products SET last_enqueued_at = NOW() WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}
