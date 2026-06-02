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
// Если подписка уже активна — переактивирует/обновляет и возвращает inserted=false.
// При вставке и при реактивации стратегия сбрасывается на дефолт (any_drop),
// first_seen_price фиксируется на текущей цене, notified сбрасывается.
func (r *SubscriptionRepo) Upsert(ctx context.Context, userID, productID int64, baselinePrice float64) (*domain.Subscription, bool, error) {
	const q = `
		INSERT INTO subscriptions (user_id, product_id, baseline_price, first_seen_price)
		VALUES ($1, $2, $3, $3)
		ON CONFLICT (user_id, product_id) DO UPDATE
			SET active           = TRUE,
			    baseline_price   = EXCLUDED.baseline_price,
			    first_seen_price = EXCLUDED.baseline_price,
			    trigger_type     = 'any_drop',
			    target_price     = NULL,
			    discount_pct     = NULL,
			    notified         = FALSE,
			    updated_at       = NOW()
		RETURNING id, user_id, product_id, baseline_price, first_seen_price,
		          trigger_type, target_price, discount_pct, notified,
		          active, created_at, updated_at,
		          (xmax = 0) AS inserted`

	s := &domain.Subscription{}
	var inserted bool
	err := withSpan(ctx, "upsert_subscription", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, userID, productID, baselinePrice).
			Scan(&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.FirstSeenPrice,
				&s.TriggerType, &s.TargetPrice, &s.DiscountPct, &s.Notified,
				&s.Active, &s.CreatedAt, &s.UpdatedAt, &inserted)
	})
	if err != nil {
		return nil, false, err
	}
	return s, inserted, nil
}

func (r *SubscriptionRepo) GetByID(ctx context.Context, id int64) (*domain.Subscription, error) {
	const q = `
		SELECT id, user_id, product_id, baseline_price, first_seen_price,
		       trigger_type, target_price, discount_pct, notified,
		       active, created_at, updated_at
		FROM subscriptions WHERE id = $1`

	s := &domain.Subscription{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.FirstSeenPrice,
			&s.TriggerType, &s.TargetPrice, &s.DiscountPct, &s.Notified,
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
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.first_seen_price,
		       s.trigger_type, s.target_price, s.discount_pct, s.notified,
		       s.active, s.created_at, s.updated_at,
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
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.FirstSeenPrice,
			&s.TriggerType, &s.TargetPrice, &s.DiscountPct, &s.Notified,
			&s.Active, &s.CreatedAt, &s.UpdatedAt,
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
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.first_seen_price,
		       s.trigger_type, s.target_price, s.discount_pct, s.notified,
		       s.active, s.created_at, s.updated_at,
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
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.FirstSeenPrice,
			&s.TriggerType, &s.TargetPrice, &s.DiscountPct, &s.Notified,
			&s.Active, &s.CreatedAt, &s.UpdatedAt,
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
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.first_seen_price,
		       s.trigger_type, s.target_price, s.discount_pct, s.notified,
		       s.active, s.created_at, s.updated_at,
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
			&s.ID, &s.UserID, &s.ProductID, &s.BaselinePrice, &s.FirstSeenPrice,
			&s.TriggerType, &s.TargetPrice, &s.DiscountPct, &s.Notified,
			&s.Active, &s.CreatedAt, &s.UpdatedAt,
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

// UpdateBaseline — зафиксировать цену последнего уведомления.
// Вызывается ТОЛЬКО после успешной отправки уведомления, поэтому здесь же
// помечаем подписку как notified=TRUE (переводит в фазу повторных срабатываний).
func (r *SubscriptionRepo) UpdateBaseline(ctx context.Context, id int64, newPrice float64) error {
	const q = `
		UPDATE subscriptions
		SET baseline_price = $2, notified = TRUE, updated_at = NOW()
		WHERE id = $1`

	_, err := r.db.Exec(ctx, q, id, newPrice)
	return err
}

// SetTrigger — сменить стратегию триггера товарной подписки.
// target и pct передаются только для соответствующих типов (иначе nil).
// CHECK-констрейнты в БД гарантируют согласованность.
func (r *SubscriptionRepo) SetTrigger(ctx context.Context, id int64, trigger string, target *float64, pct *int16) error {
	const q = `
		UPDATE subscriptions
		SET trigger_type = $2, target_price = $3, discount_pct = $4, updated_at = NOW()
		WHERE id = $1`

	tag, err := r.db.Exec(ctx, q, id, trigger, target, pct)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}
