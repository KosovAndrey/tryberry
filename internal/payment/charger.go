package payment

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// Charger — периодический шедулер автосписаний по подпискам. Два прохода:
// предупреждение о скором списании и собственно списание (с dunning-ретраями).
// Само продление плана происходит на вебхуке провайдера (ResultURL → Applier),
// charger лишь инициирует списание.
type Charger struct {
	provider Provider
	billing  *postgres.BillingSubscriptionRepo
	payments *postgres.PaymentRepo
	users    *postgres.UserRepo
	notify   Notifier
	log      *slog.Logger
}

func NewCharger(
	provider Provider,
	billing *postgres.BillingSubscriptionRepo,
	payments *postgres.PaymentRepo,
	users *postgres.UserRepo,
	notify Notifier,
	log *slog.Logger,
) *Charger {
	return &Charger{provider: provider, billing: billing, payments: payments, users: users, notify: notify, log: log}
}

// Run крутит тики с интервалом interval, пока жив ctx. Первый тик — сразу.
func (c *Charger) Run(ctx context.Context, interval time.Duration) {
	c.log.Info("billing charger started", "interval", interval, "provider", c.provider.Name())
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		c.tick(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

func (c *Charger) tick(ctx context.Context) {
	c.preNotice(ctx)
	c.charge(ctx)
}

// preNotice предупреждает о предстоящем списании (один раз на период).
func (c *Charger) preNotice(ctx context.Context) {
	now := time.Now()
	subs, err := c.billing.ListForPreNotice(ctx, now.Add(domain.SubPreNoticeLead))
	if err != nil {
		c.log.Error("charger: list pre-notice", "err", err)
		return
	}
	for _, s := range subs {
		buyer, err := c.users.GetByID(ctx, s.UserID)
		if err != nil {
			c.log.Error("charger: load buyer (pre-notice)", "user_id", s.UserID, "err", err)
			continue
		}
		c.notify.SubscriptionChargeUpcoming(ctx, buyer, s.Plan, s.AmountKopecks, s.NextChargeAt)
		if err := c.billing.MarkPreNoticeSent(ctx, s.ID, now); err != nil {
			c.log.Error("charger: mark pre-notice", "sub_id", s.ID, "err", err)
		}
	}
}

// charge инициирует автосписания по подпискам, которым подошёл срок. Отбор
// атомарно переносит next_charge_at на retryAt (in-flight + расписание ретрая);
// успех подтвердится по ResultURL (Applier.MarkRenewed выставит реальный срок).
func (c *Charger) charge(ctx context.Context) {
	now := time.Now()
	retryAt := now.Add(domain.SubChargeRetryInterval)
	subs, err := c.billing.ClaimDueForCharge(ctx, now, retryAt)
	if err != nil {
		c.log.Error("charger: claim due", "err", err)
		return
	}
	for _, s := range subs {
		c.chargeOne(ctx, s)
	}
}

func (c *Charger) chargeOne(ctx context.Context, s *domain.BillingSubscription) {
	p, _ := domain.PlanByName(s.Plan)
	description := fmt.Sprintf("Подписка TryberryBot — тариф %s, %d дней", p.Title, domain.PurchaseDays)

	// Строка платежа-продления: её id — новый InvId для Робокассы.
	paymentID, err := c.payments.Create(ctx, domain.Payment{
		UserID:                s.UserID,
		IdempotenceKey:        fmt.Sprintf("renewal-%d-%d", s.ID, time.Now().UnixNano()),
		Provider:              c.provider.Name(),
		Kind:                  domain.PayKindSubRenewal,
		Plan:                  s.Plan,
		Days:                  domain.PurchaseDays,
		AmountKopecks:         s.AmountKopecks,
		BillingSubscriptionID: &s.ID,
	})
	if err != nil {
		c.log.Error("charger: create renewal payment", "sub_id", s.ID, "err", err)
		return // next_charge_at уже на retryAt — повторим на следующем тике
	}

	err = c.provider.ChargeRecurring(ctx, RecurringParams{
		PaymentID:         paymentID,
		PreviousInvoiceID: s.RecurringInvoiceID,
		Plan:              s.Plan,
		Description:       description,
		AmountKopecks:     s.AmountKopecks,
	})
	if err != nil {
		c.onChargeFailed(ctx, s, paymentID, err)
		return
	}
	c.log.Info("charger: recurring charge initiated", "sub_id", s.ID, "payment_id", paymentID)
	// Успех подтвердится по ResultURL → Applier продлит план и MarkRenewed.
}

// onChargeFailed — синхронный отказ списания: dunning (ретрай) или остановка.
func (c *Charger) onChargeFailed(ctx context.Context, s *domain.BillingSubscription, paymentID int64, cause error) {
	c.log.Warn("charger: recurring charge failed", "sub_id", s.ID, "payment_id", paymentID, "err", cause)
	if err := c.payments.MarkCanceled(ctx, paymentID); err != nil {
		c.log.Error("charger: cancel failed payment", "payment_id", paymentID, "err", err)
	}
	expired, err := c.billing.RecordChargeFailure(ctx, s.ID, domain.SubMaxChargeFails)
	if err != nil {
		c.log.Error("charger: record failure", "sub_id", s.ID, "err", err)
		return
	}
	buyer, err := c.users.GetByID(ctx, s.UserID)
	if err != nil {
		c.log.Error("charger: load buyer (failed)", "user_id", s.UserID, "err", err)
		return
	}
	c.notify.SubscriptionPaymentFailed(ctx, buyer, s.Plan, !expired)
}
