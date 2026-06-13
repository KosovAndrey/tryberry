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

// Create — заводит платёж в статусе pending и возвращает его id. yk_payment_id
// проставляется позже (SetYKID) после ответа ЮKassa на create.
func (r *PaymentRepo) Create(ctx context.Context, p domain.Payment) (int64, error) {
	var id int64
	err := r.db.QueryRow(ctx,
		`INSERT INTO payments (user_id, idempotence_key, plan, days, amount_kopecks, promo_code_id)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id`,
		p.UserID, p.IdempotenceKey, p.Plan, p.Days, p.AmountKopecks, p.PromoCodeID).Scan(&id)
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

// AppliedPayment — результат успешного перехода платежа в succeeded.
type AppliedPayment struct {
	UserID        int64
	Plan          string
	Days          int
	AmountKopecks int64
	ExpiresAt     time.Time
	PromoCodeID   *int64
}

// MarkSucceeded атомарно и идемпотентно применяет оплату: переводит платёж
// pending→succeeded (только первый переход проходит — защита от повторных
// вебхуков и переотправок Kafka) и продлевает план пользователю. Возвращает
// applied=false, если платёж уже применён, отменён или не найден.
//
// Погашение discount-кода и реферальную награду НЕ делает — это best-effort
// шаги в консьюмере (их сбой не должен откатывать денежный путь).
func (r *PaymentRepo) MarkSucceeded(ctx context.Context, ykID string, now time.Time) (AppliedPayment, bool, error) {
	var out AppliedPayment

	tx, err := r.db.Begin(ctx)
	if err != nil {
		return out, false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck // rollback после commit — no-op

	// Переход статуса под условием — применяет план только первый.
	var days int
	err = tx.QueryRow(ctx,
		`UPDATE payments SET status = 'succeeded', paid_at = $2
		 WHERE yk_payment_id = $1 AND status = 'pending'
		 RETURNING user_id, plan, days, amount_kopecks, promo_code_id`,
		ykID, now).Scan(&out.UserID, &out.Plan, &days, &out.AmountKopecks, &out.PromoCodeID)
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
	return out, true, nil
}
