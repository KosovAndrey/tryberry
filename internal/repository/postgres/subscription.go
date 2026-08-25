package postgres

import (
	"context"
	"errors"
	"time"

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

// UpsertOutOfStock — подписать на товар БЕЗ активного оффера. Стратегия по
// умолчанию back_in_stock («уведомить, когда появится в наличии»).
//
// Последняя известная цена (baseline/first_seen — опора для below_target/
// discount_pct и строки «последняя цена X») берётся ИЗ НАШЕЙ ИСТОРИИ, а не от
// маркетплейса. Раньше её передавали снаружи из Result.Price, куда скрейпер клал
// «справочную» цену из стейта МП, — и она врала: у Я.Маркета на одном SKU много
// продавцов, стейт при OOS отдаёт цену оффера, по которому купить нельзя
// (Xiaomi Smartmi Air Purifier 2, 2026-07-15: бот показал «последняя цена
// 11 049 ₽», хотя товар продавался по 12 664 ₽ и такой цены в истории нет вовсе).
// price_history — цены, которые мы САМИ видели активными, поэтому источник тут он.
// Истории нет (первый скрейп застал товар уже без оффера) → 0, и это честно:
// вызывающий покажет «цена появится, когда товар вернётся».
func (r *SubscriptionRepo) UpsertOutOfStock(ctx context.Context, userID, productID int64) (*domain.Subscription, bool, error) {
	const q = `
		WITH last AS (
			SELECT COALESCE((
				SELECT price FROM price_history
				 WHERE product_id = $2
				 ORDER BY recorded_at DESC
				 LIMIT 1
			), 0) AS price
		)
		INSERT INTO subscriptions (user_id, product_id, baseline_price, first_seen_price, trigger_type)
		SELECT $1, $2, last.price, last.price, 'back_in_stock' FROM last
		ON CONFLICT (user_id, product_id) DO UPDATE
			SET active           = TRUE,
			    baseline_price   = EXCLUDED.baseline_price,
			    first_seen_price = EXCLUDED.first_seen_price,
			    trigger_type     = 'back_in_stock',
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
	err := withSpan(ctx, "upsert_subscription_oos", func(ctx context.Context) error {
		return r.db.QueryRow(ctx, q, userID, productID).
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
		       p.public_id,
		       p.marketplace,
		       COALESCE((
		           SELECT ph.price FROM price_history ph
		           WHERE ph.product_id = s.product_id
		           ORDER BY ph.recorded_at DESC
		           LIMIT 1
		       ), 0) AS current_price,
		       p.in_stock
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
			&s.ProductPublicID,
			&s.ProductMarketplace,
			&s.CurrentPrice,
			&s.ProductInStock,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// GetActiveByProductIDWithTelegramID — активные подписки на товар + telegram_id,
// план владельца и last_evaluated_at (для throttle оценки по интервалу тарифа).
func (r *SubscriptionRepo) GetActiveByProductIDWithTelegramID(ctx context.Context, productID int64) ([]*domain.Subscription, error) {
	const q = `
		SELECT s.id, s.user_id, s.product_id, s.baseline_price, s.first_seen_price,
		       s.trigger_type, s.target_price, s.discount_pct, s.notified,
		       s.active, s.created_at, s.updated_at, s.last_evaluated_at,
		       p.name, p.url, COALESCE(p.image_url, ''), p.public_id, p.marketplace,
		       COALESCE(u.telegram_id, 0), u.plan, u.plan_expires_at
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
			&s.Active, &s.CreatedAt, &s.UpdatedAt, &s.LastEvaluatedAt,
			&s.ProductName, &s.ProductURL, &s.ProductImageURL, &s.ProductPublicID, &s.ProductMarketplace,
			&s.TelegramID, &s.OwnerPlan, &s.OwnerPlanExpiresAt,
		); err != nil {
			return nil, err
		}
		subs = append(subs, s)
	}
	return subs, rows.Err()
}

// MarkEvaluated — отметить, что товарная подписка оценена сейчас (throttle).
func (r *SubscriptionRepo) MarkEvaluated(ctx context.Context, id int64) error {
	const q = `UPDATE subscriptions SET last_evaluated_at = NOW() WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id)
	return err
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

// Deactivate гасит подписку ТОЛЬКО если она принадлежит userID — id приходит из
// callback_data/команды пользователя (недоверенный ввод), поэтому фильтр по
// владельцу обязателен, иначе IDOR: чужую подписку можно отменить по её id.
func (r *SubscriptionRepo) Deactivate(ctx context.Context, id, userID int64) error {
	const q = `
		UPDATE subscriptions
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
// userID обязателен: id берётся из callback_data пользователя, фильтр по
// владельцу не даёт менять стратегию чужой подписки (IDOR).
// MaxActivePerUser — наибольшее число активных товарных подписок у одного юзера.
// Сигнал для решения о бандлинге уведомлений (metrics.MaxActiveSubsPerUser).
func (r *SubscriptionRepo) MaxActivePerUser(ctx context.Context) (int, error) {
	const q = `
		SELECT COALESCE(MAX(cnt), 0) FROM (
			SELECT count(*) AS cnt FROM subscriptions
			WHERE active = TRUE GROUP BY user_id
		) t`
	var n int
	err := r.db.QueryRow(ctx, q).Scan(&n)
	return n, err
}

func (r *SubscriptionRepo) SetTrigger(ctx context.Context, id, userID int64, trigger string, target *float64, pct *int16) error {
	const q = `
		UPDATE subscriptions
		SET trigger_type = $2, target_price = $3, discount_pct = $4, updated_at = NOW()
		WHERE id = $1 AND user_id = $5`

	tag, err := r.db.Exec(ctx, q, id, trigger, target, pct, userID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return domain.ErrNotFound
	}
	return nil
}

// ── Grace-период плана (reconciler в notifier) ───────────────────────────────

// ProductSubForReconcile — товарная подписка с планом владельца для reconcile.
// Лимиты (MaxProduct) живут в коде (domain.Plans), поэтому решения о паузе/возврате
// считаем в Go. ActiveCount заполняется только при выборке паузных (для возврата
// ровно до лимита с учётом уже активных), иначе 0.
type ProductSubForReconcile struct {
	ID            int64
	UserID        int64
	TelegramID    int64
	Plan          string
	PlanExpiresAt *time.Time
	CreatedAt     time.Time
	ActiveCount   int
}

// ListActiveOfExpiredUsers — активные товарные подписки пользователей с истёкшим
// планом (effective = free, MaxProduct=10 → избыток сверх лимита на паузу).
// Упорядочено (user_id, created_at): вызывающий оставляет старые, гасит лишние.
func (r *SubscriptionRepo) ListActiveOfExpiredUsers(ctx context.Context) ([]ProductSubForReconcile, error) {
	const q = `
		SELECT s.id, s.user_id, COALESCE(u.telegram_id, 0), u.plan, u.plan_expires_at, s.created_at
		FROM subscriptions s
		JOIN users u ON u.id = s.user_id
		WHERE s.active = TRUE AND s.paused_at IS NULL
		  AND u.plan_expires_at IS NOT NULL AND u.plan_expires_at < NOW()
		ORDER BY s.user_id, s.created_at`

	rows, err := r.db.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProductSubForReconcile
	for rows.Next() {
		var p ProductSubForReconcile
		if err := rows.Scan(&p.ID, &p.UserID, &p.TelegramID, &p.Plan, &p.PlanExpiresAt, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// PauseProductSubs ставит подписки на паузу (active=FALSE, paused_at=NOW()).
func (r *SubscriptionRepo) PauseProductSubs(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `
		UPDATE subscriptions
		SET active = FALSE, paused_at = NOW(), updated_at = NOW()
		WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}

// ListPausedWithinGrace — паузные товарные подписки в пределах grace
// (paused_at >= cutoff), с планом владельца и текущим числом активных подписок
// (чтобы вернуть ровно до лимита нового плана). Упорядочено (user_id, created_at).
func (r *SubscriptionRepo) ListPausedWithinGrace(ctx context.Context, cutoff time.Time) ([]ProductSubForReconcile, error) {
	const q = `
		SELECT s.id, s.user_id, COALESCE(u.telegram_id, 0), u.plan, u.plan_expires_at, s.created_at,
		       (SELECT count(*) FROM subscriptions a WHERE a.user_id = s.user_id AND a.active) AS active_count
		FROM subscriptions s
		JOIN users u ON u.id = s.user_id
		WHERE s.paused_at IS NOT NULL AND s.paused_at >= $1
		ORDER BY s.user_id, s.created_at`

	rows, err := r.db.Query(ctx, q, cutoff)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ProductSubForReconcile
	for rows.Next() {
		var p ProductSubForReconcile
		if err := rows.Scan(&p.ID, &p.UserID, &p.TelegramID, &p.Plan, &p.PlanExpiresAt, &p.CreatedAt, &p.ActiveCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// Reactivate возвращает паузные подписки в работу (active=TRUE, paused_at=NULL).
func (r *SubscriptionRepo) Reactivate(ctx context.Context, ids []int64) error {
	if len(ids) == 0 {
		return nil
	}
	const q = `
		UPDATE subscriptions
		SET active = TRUE, paused_at = NULL, updated_at = NOW()
		WHERE id = ANY($1)`
	_, err := r.db.Exec(ctx, q, ids)
	return err
}

// RestorePausedForUser реактивирует до limit самых старых паузных товарных
// подписок юзера в пределах grace (paused_at >= cutoff). Для мгновенного возврата
// при покупке/выдаче плана. limit<=0 — no-op. Возвращает число восстановленных.
func (r *SubscriptionRepo) RestorePausedForUser(ctx context.Context, userID int64, limit int, cutoff time.Time) (int64, error) {
	if limit <= 0 {
		return 0, nil
	}
	const q = `
		WITH to_restore AS (
			SELECT id FROM subscriptions
			WHERE user_id = $1 AND paused_at IS NOT NULL AND paused_at >= $2
			ORDER BY created_at
			LIMIT $3
		)
		UPDATE subscriptions s
		SET active = TRUE, paused_at = NULL, updated_at = NOW()
		FROM to_restore t
		WHERE s.id = t.id`
	tag, err := r.db.Exec(ctx, q, userID, cutoff, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteExpiredGraceProductSubs удаляет паузные подписки старше grace
// (paused_at < cutoff). У notifications нет ON DELETE CASCADE на subscription_id,
// поэтому зависимые строки удаляем в одной транзакции: сначала notifications,
// затем subscriptions. Возвращает число удалённых подписок.
func (r *SubscriptionRepo) DeleteExpiredGraceProductSubs(ctx context.Context, cutoff time.Time) (int64, error) {
	tx, err := r.db.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	const delNotif = `
		DELETE FROM notifications
		WHERE subscription_id IN (
			SELECT id FROM subscriptions WHERE paused_at IS NOT NULL AND paused_at < $1
		)`
	if _, err := tx.Exec(ctx, delNotif, cutoff); err != nil {
		return 0, err
	}

	const delSubs = `DELETE FROM subscriptions WHERE paused_at IS NOT NULL AND paused_at < $1`
	tag, err := tx.Exec(ctx, delSubs, cutoff)
	if err != nil {
		return 0, err
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}
