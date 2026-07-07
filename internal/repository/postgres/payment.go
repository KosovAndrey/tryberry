package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

type PaymentRepo struct {
	db *pgxpool.Pool
}

func NewPaymentRepo(db *pgxpool.Pool) *PaymentRepo {
	return &PaymentRepo{db: db}
}

// Create — заводит платёж в статусе pending и возвращает его id. Внешний id
// платежа (yk_payment_id для ЮKassa) проставляется позже через SetYKID; для
// Робокассы внешний id == возвращённый здесь payments.id (InvId).
// Пустые provider/kind подставляются дефолтами схемы (yookassa/onetime).
func (r *PaymentRepo) Create(ctx context.Context, p domain.Payment) (int64, error) {
	provider := p.Provider
	if provider == "" {
		provider = domain.ProviderYooKassa
	}
	kind := p.Kind
	if kind == "" {
		kind = domain.PayKindOnetime
	}
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO payments (user_id, idempotence_key, provider, kind, plan, days, amount_kopecks, promo_code_id, billing_subscription_id)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
		 RETURNING id`,
		p.UserID, p.IdempotenceKey, provider, kind, p.Plan, p.Days, p.AmountKopecks, p.PromoCodeID, p.BillingSubscriptionID).Scan(&id)
	if err == nil {
		metrics.PaymentsCreated.Inc()
	}
	return id, err
}

// SetYKID — связать строку платежа с id платежа в ЮKassa (после create).
func (r *PaymentRepo) SetYKID(ctx context.Context, id int64, ykID string) error {
	_, err := r.db.Exec(ctx,
		`UPDATE payments SET yk_payment_id = $2 WHERE id = $1`, id, ykID)
	return err
}

// MarkCanceled помечает pending-платёж отменённым (например, провайдер отклонил
// автосписание сразу). Только из pending — succeeded не трогаем.
func (r *PaymentRepo) MarkCanceled(ctx context.Context, id int64) error {
	_, err := r.db.Exec(ctx,
		`UPDATE payments SET status = 'canceled' WHERE id = $1 AND status = 'pending'`, id)
	return err
}

// AppliedPayment — результат успешного перехода платежа в succeeded.
type AppliedPayment struct {
	PaymentID             int64
	UserID                int64
	Plan                  string
	Kind                  string // onetime | subscription_initial | subscription_renewal
	Days                  int
	AmountKopecks         int64
	ExpiresAt             time.Time
	PromoCodeID           *int64
	BillingSubscriptionID *int64 // для рекуррентных платежей
}

// ConfirmInfo — user_id и сумма платежа по его id (для вебхука: получить ключ
// партиционирования и сверить сумму). found=false, если платежа нет.
func (r *PaymentRepo) ConfirmInfo(ctx context.Context, id int64) (userID, amountKopecks int64, found bool, err error) {
	err = r.db.QueryRow(ctx,
		`SELECT user_id, amount_kopecks FROM payments WHERE id = $1`, id).
		Scan(&userID, &amountKopecks)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, false, nil
	}
	if err != nil {
		return 0, 0, false, err
	}
	return userID, amountKopecks, true, nil
}

// MarkSucceeded атомарно и идемпотентно применяет оплату: переводит платёж
// pending→succeeded (только первый переход проходит — защита от повторных
// вебхуков и переотправок Kafka) и продлевает план пользователю. Возвращает
// applied=false, если платёж уже применён, отменён или не найден.
//
// Погашение discount-кода и реферальную награду НЕ делает — это best-effort
// шаги в консьюмере (их сбой не должен откатывать денежный путь).
func (r *PaymentRepo) MarkSucceeded(ctx context.Context, paymentID int64, now time.Time) (AppliedPayment, bool, error) {
	var out AppliedPayment

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Переход статуса под условием — применяет план только первый.
	var days int
	out.PaymentID = paymentID
	err = tx.QueryRow(ctx,
		`UPDATE payments SET status = 'succeeded', paid_at = $2
		 WHERE id = $1 AND status = 'pending'
		 RETURNING user_id, plan, kind, days, amount_kopecks, promo_code_id, billing_subscription_id`,
		paymentID, now).Scan(&out.UserID, &out.Plan, &out.Kind, &days, &out.AmountKopecks, &out.PromoCodeID, &out.BillingSubscriptionID)
	if errors.Is(err, pgx.ErrNoRows) {
		return out, false, nil // уже применён / отменён / неизвестен
	}
	if err != nil {
		return out, false, err
	}
	out.Days = days

	// Текущий план под блокировкой строки — чтобы корректно продлить.
	var plan string
	var expiresAt *time.Time
	if err := tx.QueryRow(ctx,
		`SELECT plan, plan_expires_at FROM users WHERE id = $1 FOR UPDATE`,
		out.UserID).Scan(&plan, &expiresAt); err != nil {
		return out, false, err
	}
	u := domain.User{Plan: plan, PlanExpiresAt: expiresAt}
	out.ExpiresAt = domain.ApplyPurchase(&u, out.Plan, days, now)

	if _, err := tx.Exec(ctx,
		`UPDATE users SET plan = $2, plan_expires_at = $3, plan_reminded_at = NULL
		 WHERE id = $1`, out.UserID, out.Plan, out.ExpiresAt); err != nil {
		return out, false, err
	}

	if err := tx.Commit(ctx); err != nil {
		return out, false, err
	}

	metrics.PaymentsSucceeded.Inc()
	metrics.PaymentRevenueKopecks.Add(float64(out.AmountKopecks))
	// Воронка конверсии: план до покупки (plan) → купленный (out.Plan).
	// from==to = продление; from=free/trial → to=lite/pro = целевой апгрейд.
	metrics.TariffUpgrades.WithLabelValues(plan, out.Plan).Inc()
	return out, true, nil
}
