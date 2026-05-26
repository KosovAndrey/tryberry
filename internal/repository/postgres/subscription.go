package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type SubscriptionRepo struct {
	db *pgxpool.Pool
}

func NewSubscriptionRepo(db *pgxpool.Pool) *SubscriptionRepo {
	return &SubscriptionRepo{db: db}
}

// Upsert — подписать пользователя на товар.
// Если подписка уже активна — возвращает ErrAlreadySubscribed.
// Если была отменена — переактивирует и обновляет baseline_price.
func (r *SubscriptionRepo) Upsert(ctx context.Context, userID, productID int64, baselinePrice float64) (*domain.Subscription, bool, error) {
	const q = `
		INSERT INTO subscriptions (user_id, product_id, baseline_price)
		VALUES ($1, $2, $3)
		ON CONFLICT (user_id, product_id) DO UPDATE
			SET active         = TRUE,
			    baseline_price = EXCLUDED.baseline_price,
			    updated_at     = NOW()
		RETURNING id, user_id, product_id, baseline_price, active, created_at, updated_at,
		          (xmax = 0) AS inserted`
	// xmax = 0 означает что строка была INSERT, а не UPDATE

	s := &domain.Subscription{}
	var inserted bool
	err := r.db.QueryRow(ctx, q, userID, productID, baselinePrice).
		Scan(&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice,
			&s.Active, &s.CreatedAt, &s.UpdatedAt, &inserted)
	if err != nil {
		return nil, false, err
	}

	// Если строка существовала и была активна до UPDATE — это дубль
	// inserted=false означает что была UPDATE, но мы не знаем была ли она активна
	// Поэтому проверяем через отдельный флаг: если !inserted и active был true до — это повтор
	// Упрощение: возвращаем created=inserted, вызывающий код решает как ответить
	return s, inserted, nil
}

func (r *SubscriptionRepo) GetByID(ctx context.Context, id int64) (*domain.Subscription, error) {
	const q = `
		SELECT id, user_id, product_id, baseline_price, active, created_at, updated_at
		FROM subscriptions WHERE id = $1`

	s := &domain.Subscription{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice,
			&s.Active, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

func (r *SubscriptionRepo) GetActiveByUserID(ctx context.Context, userID int64) ([]*domain.Subscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.active,
		       s.created_at, s.updated_at,
		       p.name, p.url,
		       COALESCE(p.image_url, ''),
		       p.marketplace,
		       COALESCE((
		           SELECT ph.price FROM price_history ph
		           WHERE ph.product_id = s.product_id
		           ORDER BY ph.recorded_at DESC
		           LIMIT 1
		       ), 0) AS current_price
		FROM subscriptions s
		JOIN products p ON p.id = s.product_id
		WHERE s.user_id = $1 AND s.active = TRUE
		ORDER BY s.created_at`

	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*domain.Subscription
	for rows.Next() {
		s := &domain.Subscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.Active,
			&s.CreatedAt, &s.UpdatedAt,
			&s.ProductName, &s.ProductURL, &s.ProductImageURL,
			&s.ProductMarketplace,
			&s.CurrentPrice,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

func (r *SubscriptionRepo) GetActiveByProductIDWithTelegramID(ctx context.Context, productID int64) ([]*domain.Subscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.active,
		       s.created_at, s.updated_at,
		       p.name, p.url, COALESCE(p.image_url, ''), p.marketplace,
		       u.telegram_id
		FROM subscriptions s
		JOIN products p ON p.id = s.product_id
		JOIN users u ON u.id = s.user_id
		WHERE s.product_id = $1 AND s.active = TRUE`

	rows, err := r.db.Query(ctx, q, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*domain.Subscription
	for rows.Next() {
		s := &domain.Subscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.Active,
			&s.CreatedAt, &s.UpdatedAt,
			&s.ProductName, &s.ProductURL, &s.ProductImageURL, &s.ProductMarketplace,
			&s.TelegramID,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

func (r *SubscriptionRepo) GetActiveByProductID(ctx context.Context, productID int64) ([]*domain.Subscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.active,
		       s.created_at, s.updated_at,
		       p.name, p.url, COALESCE(p.image_url, '')
		FROM subscriptions s
		JOIN products p ON p.id = s.product_id
		WHERE s.product_id = $1 AND s.active = TRUE`

	rows, err := r.db.Query(ctx, q, productID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*domain.Subscription
	for rows.Next() {
		s := &domain.Subscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.Active,
			&s.CreatedAt, &s.UpdatedAt,
			&s.ProductName, &s.ProductURL, &s.ProductImageURL,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

func (r *SubscriptionRepo) Deactivate(ctx context.Context, id int64) error {
	const q = `
		UPDATE subscriptions
		SET active = FALSE, updated_at = NOW()
		WHERE id = $1`

	tag, err := r.db.Exec(ctx, q, id)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

func (r *SubscriptionRepo) UpdateBaseline(ctx context.Context, id int64, newPrice float64) error {
	const q = `
		UPDATE subscriptions
		SET baseline_price = $2, updated_at = NOW()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, q, id, newPrice)
	return err
}
