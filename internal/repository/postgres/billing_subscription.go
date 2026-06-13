package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

// BillingSubscriptionRepo — рекуррентные подписки (НЕ путать с SubscriptionRepo,
// та про товарные подписки на снижение цены).
type BillingSubscriptionRepo struct {
	db *pgxpool.Pool
}

func NewBillingSubscriptionRepo(db *pgxpool.Pool) *BillingSubscriptionRepo {
	return &BillingSubscriptionRepo{db: db}
}

// Activate создаёт активную подписку или переактивирует уже существующую
// активную того же юзера (повторное оформление: новая связка/цена/срок). Partial
// unique (user_id) WHERE active гарантирует не более одной активной на юзера.
// Вызывается на применении первого платежа подписки.
func (r *BillingSubscriptionRepo) Activate(ctx context.Context, s domain.BillingSubscription) (int64, error) {
	const q = `
		INSERT INTO billing_subscriptions
			(user_id, plan, status, amount_kopecks, recurring_invoice_id, next_charge_at, last_payment_id)
		VALUES ($1, $2, 'active', $3, $4, $5, $6)
		ON CONFLICT (user_id) WHERE status = 'active'
		DO UPDATE SET
			plan                 = EXCLUDED.plan,
			amount_kopecks       = EXCLUDED.amount_kopecks,
			recurring_invoice_id = EXCLUDED.recurring_invoice_id,
			next_charge_at       = EXCLUDED.next_charge_at,
			last_payment_id      = EXCLUDED.last_payment_id,
			fail_count           = 0,
			pre_notice_sent_at   = NULL,
			updated_at           = NOW()
		RETURNING id`
	var id int64
	err := r.db.QueryRow(ctx, q,
		s.UserID, s.Plan, s.AmountKopecks, s.RecurringInvoiceID, s.NextChargeAt, s.LastPaymentID).Scan(&id)
	return id, err
}

// GetActiveByUserID — активная или просроченная (past_due) подписка юзера, если
// есть. ErrNotFound, если нет подписки в этих статусах.
func (r *BillingSubscriptionRepo) GetActiveByUserID(ctx context.Context, userID int64) (*domain.BillingSubscription, error) {
	const q = `
		SELECT id, user_id, plan, status, amount_kopecks, recurring_invoice_id,
		       next_charge_at, pre_notice_sent_at, fail_count, last_payment_id,
		       created_at, updated_at, canceled_at
		FROM billing_subscriptions
		WHERE user_id = $1 AND status IN ('active', 'past_due')
		ORDER BY created_at DESC
		LIMIT 1`
	s := &domain.BillingSubscription{}
	err := r.db.QueryRow(ctx, q, userID).Scan(
		&s.ID, &s.UserID, &s.Plan, &s.Status, &s.AmountKopecks, &s.RecurringInvoiceID,
		&s.NextChargeAt, &s.PreNoticeSentAt, &s.FailCount, &s.LastPaymentID,
		&s.CreatedAt, &s.UpdatedAt, &s.CanceledAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, domain.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return s, nil
}

// Cancel отменяет автопродление подписки юзера (доступ к плану сохраняется до
// конца оплаченного периода — план не трогаем). Фильтр по user_id — защита от
// IDOR (id не принимаем извне, гасим по владельцу). Возвращает canceled=false,
// если активной/просроченной подписки нет.
func (r *BillingSubscriptionRepo) Cancel(ctx context.Context, userID int64) (bool, error) {
	const q = `
		UPDATE billing_subscriptions
		SET status = 'canceled', canceled_at = NOW(), updated_at = NOW()
		WHERE user_id = $1 AND status IN ('active', 'past_due')`
	tag, err := r.db.Exec(ctx, q, userID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// MarkRenewed обновляет подписку после успешного автосписания: новый срок
// (next_charge_at), сброс счётчика неудач и уведомления, статус active.
func (r *BillingSubscriptionRepo) MarkRenewed(ctx context.Context, id, lastPaymentID int64, nextChargeAt time.Time) error {
	const q = `
		UPDATE billing_subscriptions
		SET status = 'active', next_charge_at = $2, last_payment_id = $3,
		    fail_count = 0, pre_notice_sent_at = NULL, updated_at = NOW()
		WHERE id = $1`
	_, err := r.db.Exec(ctx, q, id, nextChargeAt, lastPaymentID)
	return err
}

// scanSubs — общий разбор строк billing_subscriptions.
func scanSubs(rows pgx.Rows) ([]*domain.BillingSubscription, error) {
	defer rows.Close()
	var out []*domain.BillingSubscription
	for rows.Next() {
		s := &domain.BillingSubscription{}
		if err := rows.Scan(
			&s.ID, &s.UserID, &s.Plan, &s.Status, &s.AmountKopecks, &s.RecurringInvoiceID,
			&s.NextChargeAt, &s.PreNoticeSentAt, &s.FailCount, &s.LastPaymentID,
			&s.CreatedAt, &s.UpdatedAt, &s.CanceledAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

const subCols = `id, user_id, plan, status, amount_kopecks, recurring_invoice_id,
	next_charge_at, pre_notice_sent_at, fail_count, last_payment_id,
	created_at, updated_at, canceled_at`

// ListForPreNotice — активные подписки, по которым пора предупредить о списании
// (next_charge_at <= within) и предупреждение ещё не отправляли.
func (r *BillingSubscriptionRepo) ListForPreNotice(ctx context.Context, within time.Time) ([]*domain.BillingSubscription, error) {
	rows, err := r.db.Query(ctx,
		`SELECT `+subCols+` FROM billing_subscriptions
		 WHERE status = 'active' AND pre_notice_sent_at IS NULL AND next_charge_at <= $1
		 ORDER BY next_charge_at`, within)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// MarkPreNoticeSent — отметить, что предупреждение о списании отправлено.
func (r *BillingSubscriptionRepo) MarkPreNoticeSent(ctx context.Context, id int64, now time.Time) error {
	_, err := r.db.Exec(ctx,
		`UPDATE billing_subscriptions SET pre_notice_sent_at = $2, updated_at = NOW() WHERE id = $1`,
		id, now)
	return err
}

// ClaimDueForCharge атомарно отбирает подписки к списанию (active,
// next_charge_at <= now) и сразу переносит next_charge_at на retryAt — это и
// «in-flight»-защита от повторного отбора пока ждём подтверждения по ResultURL,
// и расписание ретрая, если списание не пройдёт. Возвращает отобранные строки.
func (r *BillingSubscriptionRepo) ClaimDueForCharge(ctx context.Context, now, retryAt time.Time) ([]*domain.BillingSubscription, error) {
	rows, err := r.db.Query(ctx,
		`UPDATE billing_subscriptions
		 SET next_charge_at = $2, updated_at = NOW()
		 WHERE status IN ('active', 'past_due') AND next_charge_at <= $1
		 RETURNING `+subCols, now, retryAt)
	if err != nil {
		return nil, err
	}
	return scanSubs(rows)
}

// RecordChargeFailure фиксирует неудачное списание: fail_count++ и, если
// достигнут потолок maxFails, переводит подписку в expired (доступ истечёт по
// плану). Возвращает expired=true, если подписка остановлена окончательно.
func (r *BillingSubscriptionRepo) RecordChargeFailure(ctx context.Context, id int64, maxFails int) (bool, error) {
	var failCount int
	var status string
	err := r.db.QueryRow(ctx,
		`UPDATE billing_subscriptions
		 SET fail_count = fail_count + 1,
		     status = CASE WHEN fail_count + 1 >= $2 THEN 'expired' ELSE 'past_due' END,
		     updated_at = NOW()
		 WHERE id = $1
		 RETURNING fail_count, status`, id, maxFails).Scan(&failCount, &status)
	if err != nil {
		return false, err
	}
	return status == domain.SubStatusExpired, nil
}

// LogConsent пишет факт явного согласия на подписку (защита от чарджбэка/ЗоЗПП).
func (r *BillingSubscriptionRepo) LogConsent(ctx context.Context, c domain.SubscriptionConsent) error {
	const q = `
		INSERT INTO subscription_consents (user_id, plan, amount_kopecks, terms_version, platform)
		VALUES ($1, $2, $3, $4, $5)`
	_, err := r.db.Exec(ctx, q, c.UserID, c.Plan, c.AmountKopecks, c.TermsVersion, c.Platform)
	return err
}
