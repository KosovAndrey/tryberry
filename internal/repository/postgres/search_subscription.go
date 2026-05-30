package postgres

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

type SearchSubscriptionRepo struct {
	db *pgxpool.Pool
}

func NewSearchSubscriptionRepo(db *pgxpool.Pool) *SearchSubscriptionRepo {
	return &SearchSubscriptionRepo{db: db}
}

// Create — создать поиск-подписку.
//
// В отличие от обычных подписок, здесь нет UNIQUE(user_id, search_query_id):
// один пользователь может иметь несколько подписок на один запрос с разными
// триггерами. Поэтому обычный INSERT без ON CONFLICT.
//
// targetPrice/discountPct — указатели: nil для неприменимых к типу триггера полей.
// CHECK-констрейнты в схеме гарантируют согласованность (below_target требует
// target_price, discount_pct требует pct в 1..99).
func (r *SearchSubscriptionRepo) Create(ctx context.Context, s *domain.SearchSubscription) (*domain.SearchSubscription, error) {
	const q = `
		INSERT INTO search_subscriptions
			(user_id, search_query_id, trigger_type, target_price, discount_pct)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, user_id, search_query_id, trigger_type,
		          target_price, discount_pct, active, created_at, updated_at`

	out := &domain.SearchSubscription{}
	err := withSpan(ctx, "create_search_subscription", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q,
			s.UserID, s.SearchQueryID, string(s.TriggerType), s.TargetPrice, s.DiscountPct).
			Scan(&out.ID, &out.UserID, &out.SearchQueryID, &out.TriggerType,
				&out.TargetPrice, &out.DiscountPct, &out.Active, &out.CreatedAt, &out.UpdatedAt)
	})
	if err != nil {
		return nil, err
	}
	return out, nil
}

func (r *SearchSubscriptionRepo) GetByID(ctx context.Context, id int64) (*domain.SearchSubscription, error) {
	const q = `
		SELECT id, user_id, search_query_id, trigger_type,
		       target_price, discount_pct, active, created_at, updated_at
		FROM search_subscriptions WHERE id = $1`

	s := &domain.SearchSubscription{}
	err := r.db.QueryRow(ctx, q, id).
		Scan(&s.ID, &s.UserID, &s.SearchQueryID, &s.TriggerType,
			&s.TargetPrice, &s.DiscountPct, &s.Active, &s.CreatedAt, &s.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// GetActiveByUserID — активные поиск-подписки пользователя (для /list_search).
// JOIN search_queries для отображения текста запроса и ссылки.
func (r *SearchSubscriptionRepo) GetActiveByUserID(ctx context.Context, userID int64) ([]*domain.SearchSubscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.search_query_id, s.trigger_type,
		       s.target_price, s.discount_pct, s.active, s.created_at, s.updated_at,
		       sq.query_text, sq.normalized_url
		FROM search_subscriptions s
		JOIN search_queries sq ON sq.id = s.search_query_id
		WHERE s.user_id = $1 AND s.active = TRUE
		ORDER BY s.created_at`

	rows, err := r.db.Query(ctx, q, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*domain.SearchSubscription
	for rows.Next() {
		s := &domain.SearchSubscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.SearchQueryID, &s.TriggerType,
			&s.TargetPrice, &s.DiscountPct, &s.Active, &s.CreatedAt, &s.UpdatedAt,
			&s.QueryText, &s.NormalizedURL,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// GetActiveByQueryID — все активные подписки на запрос (для движка триггеров).
// JOIN users чтобы сразу иметь telegram_id для отправки уведомления.
func (r *SearchSubscriptionRepo) GetActiveByQueryID(ctx context.Context, queryID int64) ([]*domain.SearchSubscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.search_query_id, s.trigger_type,
		       s.target_price, s.discount_pct, s.active, s.created_at, s.updated_at,
		       u.telegram_id
		FROM search_subscriptions s
		JOIN users u ON u.id = s.user_id
		WHERE s.search_query_id = $1 AND s.active = TRUE`

	rows, err := r.db.Query(ctx, q, queryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var subs []*domain.SearchSubscription
	for rows.Next() {
		s := &domain.SearchSubscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.SearchQueryID, &s.TriggerType,
			&s.TargetPrice, &s.DiscountPct, &s.Active, &s.CreatedAt, &s.UpdatedAt,
			&s.TelegramID,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

func (r *SearchSubscriptionRepo) Deactivate(ctx context.Context, id int64) error {
	const q = `
		UPDATE search_subscriptions
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

// ── search_subscription_products (baseline для первого уведомления) ──────────

// UpsertBaseline — зафиксировать стартовую цену товара для подписки.
// ON CONFLICT DO NOTHING: first_seen_price никогда не перезаписывается —
// это «нулевая точка», от которой считается первое уведомление.
func (r *SearchSubscriptionRepo) UpsertBaseline(ctx context.Context, subID, productID int64, firstSeenPrice float64) error {
	const q = `
		INSERT INTO search_subscription_products (subscription_id, product_id, first_seen_price)
		VALUES ($1, $2, $3)
		ON CONFLICT (subscription_id, product_id) DO NOTHING`

	return withSpan(ctx, "upsert_search_baseline", func(ctx context.Context) error {
		_, err := r.db.Exec(ctx, q, subID, productID, firstSeenPrice)
		return err
	})
}

// BackfillBaselines — при создании подписки зафиксировать стартовые цены
// для всех товаров, которые уже есть в выдаче запроса.
// Возвращает число вставленных строк.
func (r *SearchSubscriptionRepo) BackfillBaselines(ctx context.Context, subID, queryID int64) (int64, error) {
	const q = `
		INSERT INTO search_subscription_products (subscription_id, product_id, first_seen_price)
		SELECT $1, sr.product_id, sr.last_price
		FROM search_results sr
		WHERE sr.search_query_id = $2
		ON CONFLICT (subscription_id, product_id) DO NOTHING`

	var n int64
	err := withSpan(ctx, "backfill_search_baselines", func(ctx context.Context) error {
		tag, e := r.db.Exec(ctx, q, subID, queryID)
		if e != nil {
			return e
		}
		n = tag.RowsAffected()
		return nil
	})
	return n, err
}

// GetBaseline — стартовая цена товара для подписки.
// ok=false если baseline ещё не зафиксирован (товара не было при создании подписки
// и он ещё не появлялся в выдаче — крайне редкий гонко-кейс).
func (r *SearchSubscriptionRepo) GetBaseline(ctx context.Context, subID, productID int64) (float64, bool, error) {
	const q = `
		SELECT first_seen_price
		FROM search_subscription_products
		WHERE subscription_id = $1 AND product_id = $2`

	var price float64
	err := r.db.QueryRow(ctx, q, subID, productID).Scan(&price)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, false, nil
	}
	if err != nil {
		return 0, false, err
	}
	return price, true, nil
}
