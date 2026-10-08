package postgres

import (
	"context"
	"errors"
	"time"

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
		       sq.query_text, sq.normalized_url, sq.marketplace, sq.last_scraped_at
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
			&s.QueryText, &s.NormalizedURL, &s.Marketplace, &s.LastScrapedAt,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// GetActiveByQueryID — все активные подписки на запрос (для движка триггеров).
// JOIN users чтобы сразу иметь telegram_id для отправки уведомления и план
// владельца (+last_evaluated_at) для throttle оценки по интервалу тарифа.
func (r *SearchSubscriptionRepo) GetActiveByQueryID(ctx context.Context, queryID int64) ([]*domain.SearchSubscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.search_query_id, s.trigger_type,
		       s.target_price, s.discount_pct, s.active, s.created_at, s.updated_at,
		       s.last_evaluated_at, COALESCE(u.telegram_id, 0), u.plan, u.plan_expires_at
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
			&s.LastEvaluatedAt, &s.TelegramID, &s.OwnerPlan, &s.OwnerPlanExpiresAt,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// MarkEvaluated — отметить, что подписка оценена сейчас (throttle уведомлений).
func (r *SearchSubscriptionRepo) MarkEvaluated(ctx context.Context, id int64) error {
	const q = `UPDATE search_subscriptions SET last_evaluated_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id)
	return err
}

// userID обязателен: id приходит из callback_data пользователя, фильтр по
// владельцу не даёт отменить чужую поиск-подписку по её id (IDOR).
func (r *SearchSubscriptionRepo) Deactivate(ctx context.Context, id, userID int64) error {
	const q = `
		UPDATE search_subscriptions
		SET active = FALSE, updated_at = NOW()
		WHERE id = $1 AND user_id = $2`

	tag, err := r.db.Exec(ctx, q, id, userID)
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

// ── Grace-период плана (reconciler в notifier) ───────────────────────────────

// PausedSearchSub — паузная подписка с планом владельца. Лимиты (MaxSearch) живут
// в коде (domain.Plans), поэтому решение о восстановлении считаем в Go, не в SQL.
type PausedSearchSub struct {
	ID            int64
	UserID        int64
	Plan          string
	PlanExpiresAt *time.Time
	CreatedAt     time.Time
	ActiveCount   int // заполняется только в ListPausedWithinGrace, иначе 0
}

// ListActiveOfExpiredUsers — активные поиск-подписки пользователей с истёкшим
// планом (effective = free, MaxSearch=1). Упорядочено (user_id, created_at):
// вызывающий оставляет старейшие в пределах лимита и гасит избыток.
//
// NB: раньше здесь был PauseExpiredSearchSubs, гасивший ВСЕ поиски истёкших
// (эпоха free.MaxSearch=0). После открытия фри-поиска restore на том же тике
// возвращал один поиск, следующий тик снова гасил его — бесконечный цикл
// пауза→возврат с повторным «Тариф закончился» раз в reconcile-интервал.
func (r *SearchSubscriptionRepo) ListActiveOfExpiredUsers(ctx context.Context) ([]PausedSearchSub, error) {
	const q = `
		SELECT s.id, s.user_id, u.plan, u.plan_expires_at, s.created_at
		FROM search_subscriptions s
		JOIN users u ON u.id = s.user_id
		WHERE s.active = TRUE AND s.paused_at IS NULL
		  AND u.plan_expires_at IS NOT NULL AND u.plan_expires_at < NOW()
		ORDER BY s.user_id, s.created_at`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PausedSearchSub
	for rows.Next() {
		var p PausedSearchSub
		if err := rows.Scan(&p.ID, &p.UserID, &p.Plan, &p.PlanExpiresAt, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PauseSearchSubs ставит поиск-подписки на паузу (active=FALSE, paused_at=NOW()).
// Повторно не трогает уже паузные (paused_at сохраняет начало grace).
func (r *SearchSubscriptionRepo) PauseSearchSubs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `
		UPDATE search_subscriptions
		SET active = FALSE, paused_at = NOW(), updated_at = NOW()
		WHERE id = ANY($1) AND paused_at IS NULL`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}

// ListPausedWithinGrace возвращает паузные подписки, ещё не вышедшие из grace
// (paused_at >= cutoff), вместе с планом владельца и числом его уже активных
// поисков. Упорядочено по (user_id, created_at), чтобы вызывающий мог
// группировать по юзеру и восстанавливать самые старые до (MaxSearch − активные).
func (r *SearchSubscriptionRepo) ListPausedWithinGrace(ctx context.Context, cutoff time.Time) ([]PausedSearchSub, error) {
	const q = `
		SELECT s.id, s.user_id, u.plan, u.plan_expires_at, s.created_at,
		       (SELECT count(*) FROM search_subscriptions a WHERE a.user_id = s.user_id AND a.active) AS active_count
		FROM search_subscriptions s
		JOIN users u ON u.id = s.user_id
		WHERE s.paused_at IS NOT NULL AND s.paused_at >= $1
		ORDER BY s.user_id, s.created_at`

	rows, err := r.db.Query(ctx, q, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []PausedSearchSub
	for rows.Next() {
		var p PausedSearchSub
		if err := rows.Scan(&p.ID, &p.UserID, &p.Plan, &p.PlanExpiresAt, &p.CreatedAt, &p.ActiveCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Reactivate возвращает паузные подписки в работу (active=TRUE, paused_at=NULL).
func (r *SearchSubscriptionRepo) Reactivate(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `
		UPDATE search_subscriptions
		SET active = TRUE, paused_at = NULL, updated_at = NOW()
		WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}

// RestorePausedForUser реактивирует до limit самых старых паузных поиск-подписок
// юзера в пределах grace (paused_at >= cutoff). Для мгновенного возврата при
// покупке/выдаче плана (reconciler сделал бы это на ближайшем тике). limit<=0 —
// no-op. Возвращает число восстановленных.
func (r *SearchSubscriptionRepo) RestorePausedForUser(ctx context.Context, userID int64, limit int, cutoff time.Time) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	const q = `
		WITH to_restore AS (
			SELECT id FROM search_subscriptions
			WHERE user_id = $1 AND paused_at IS NOT NULL AND paused_at >= $2
			ORDER BY created_at
			LIMIT $3
		)
		UPDATE search_subscriptions s
		SET active = TRUE, paused_at = NULL, updated_at = NOW()
		FROM to_restore t
		WHERE s.id = t.id`
	tag, err := r.db.Exec(ctx, q, userID, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredGraceSearchSubs удаляет паузные подписки старше grace
// (paused_at < cutoff). FK ON DELETE CASCADE сам чистит baseline
// (search_subscription_products) и историю (search_notifications).
// Возвращает число удалённых строк.
func (r *SearchSubscriptionRepo) DeleteExpiredGraceSearchSubs(ctx context.Context, cutoff time.Time) (int64, error) {
	const q = `DELETE FROM search_subscriptions WHERE paused_at IS NOT NULL AND paused_at < $1`
	tag, err := r.db.Exec(ctx, q, cutoff)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
