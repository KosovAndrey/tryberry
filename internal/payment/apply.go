package payment

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
)

// Notifier шлёт пользователям сообщения об оплате. Реализуется в bot-worker
// (роутинг TG/VK), чтобы пакет payment не зависел от ботов.
type Notifier interface {
	// PaymentSucceeded — покупателю: тариф активирован до expiresAt.
	PaymentSucceeded(ctx context.Context, buyer *domain.User, plan string, expiresAt time.Time)
	// ReferralPaid — рефереру: награда за оплату друга friendName.
	ReferralPaid(ctx context.Context, referrer *domain.User, friendName string, days int, setPlan bool)
}

// Applier применяет подтверждённую оплату: продлевает план (идемпотентно),
// заводит/продлевает подписку, гасит discount-код, начисляет реферальную
// награду paid и уведомляет.
type Applier struct {
	payments  *postgres.PaymentRepo
	promos    *postgres.PromoRepo
	referrals *postgres.ReferralRepo
	users     *postgres.UserRepo
	billing   *postgres.BillingSubscriptionRepo // рекуррентные подписки
	discounts *redisrepo.DiscountStore          // может быть nil
	notify    Notifier
	log       *slog.Logger
}

func NewApplier(
	payments *postgres.PaymentRepo,
	promos *postgres.PromoRepo,
	referrals *postgres.ReferralRepo,
	users *postgres.UserRepo,
	billing *postgres.BillingSubscriptionRepo,
	discounts *redisrepo.DiscountStore,
	notify Notifier,
	log *slog.Logger,
) *Applier {
	return &Applier{payments: payments, promos: promos, referrals: referrals,
		users: users, billing: billing, discounts: discounts, notify: notify, log: log}
}

// Apply обрабатывает одно событие подтверждённой оплаты. Возврат ошибки →
// сообщение в Kafka не коммитится и будет переотправлено (применение
// идемпотентно: MarkSucceeded пропустит уже применённый платёж).
func (a *Applier) Apply(ctx context.Context, ev ConfirmedEvent) error {
	now := time.Now()

	applied, ok, err := a.payments.MarkSucceeded(ctx, ev.PaymentID, now)
	if err != nil {
		return err // временный сбой БД — пусть Kafka переотправит
	}
	if !ok {
		return nil // уже применён / отменён / неизвестен — идемпотентно
	}

	buyer, err := a.users.GetByID(ctx, applied.UserID)
	if err != nil {
		// План уже продлён (это главное). Уведомление и реф.награду пропускаем.
		a.log.Error("payment apply: load buyer", "user_id", applied.UserID, "err", err)
		return nil
	}

	a.log.Info("payment applied",
		"user_id", buyer.ID, "plan", applied.Plan, "amount_kopecks", applied.AmountKopecks,
		"expires_at", applied.ExpiresAt, "payment_id", ev.PaymentID)

	a.notify.PaymentSucceeded(ctx, buyer, applied.Plan, applied.ExpiresAt)

	a.applySubscription(ctx, applied)

	// Скидка зафиксирована в платеже — снимаем «ожидающую» и гасим код.
	if a.discounts != nil {
		if err := a.discounts.Del(ctx, buyer.ID); err != nil {
			a.log.Warn("payment apply: clear pending discount", "user_id", buyer.ID, "err", err)
		}
	}
	if applied.PromoCodeID != nil {
		if err := a.promos.RedeemDiscount(ctx, *applied.PromoCodeID, buyer.ID); err != nil &&
			!errors.Is(err, domain.ErrPromoExhausted) && !errors.Is(err, domain.ErrPromoAlreadyRedeemed) {
			a.log.Error("payment apply: redeem discount", "code_id", *applied.PromoCodeID, "user_id", buyer.ID, "err", err)
		}
	}

	a.rewardReferrer(ctx, buyer, now)
	return nil
}

// applySubscription заводит подписку при первом платеже и продлевает её при
// автосписании. next_charge_at ставим за SubChargeLeadTime до истечения плана,
// чтобы новый период начинался без разрыва. Best-effort: план уже продлён,
// сбой подписки не откатывает оплату (на следующем тике reconcile поправит).
func (a *Applier) applySubscription(ctx context.Context, p postgres.AppliedPayment) {
	if a.billing == nil {
		return
	}
	switch p.Kind {
	case domain.PayKindSubInitial:
		nextCharge := p.ExpiresAt.Add(-domain.SubChargeLeadTime)
		id, err := a.billing.Activate(ctx, domain.BillingSubscription{
			UserID:             p.UserID,
			Plan:               p.Plan,
			AmountKopecks:      domain.SubPriceKopecks(p.Plan),
			RecurringInvoiceID: p.PaymentID, // InvId первого платежа = ключ рекуррента
			NextChargeAt:       nextCharge,
			LastPaymentID:      &p.PaymentID,
		})
		if err != nil {
			a.log.Error("apply: activate subscription", "user_id", p.UserID, "payment_id", p.PaymentID, "err", err)
			return
		}
		a.log.Info("subscription activated", "user_id", p.UserID, "sub_id", id, "next_charge_at", nextCharge)

	case domain.PayKindSubRenewal:
		if p.BillingSubscriptionID == nil {
			a.log.Error("apply: renewal without subscription id", "payment_id", p.PaymentID)
			return
		}
		nextCharge := p.ExpiresAt.Add(-domain.SubChargeLeadTime)
		if err := a.billing.MarkRenewed(ctx, *p.BillingSubscriptionID, p.PaymentID, nextCharge); err != nil {
			a.log.Error("apply: mark renewed", "sub_id", *p.BillingSubscriptionID, "payment_id", p.PaymentID, "err", err)
			return
		}
		a.log.Info("subscription renewed", "sub_id", *p.BillingSubscriptionID, "next_charge_at", nextCharge)
	}
}

// rewardReferrer начисляет рефереру покупателя награду за событие paid
// (идемпотентно: UNIQUE (referee, event) + потолок за год). Best-effort.
func (a *Applier) rewardReferrer(ctx context.Context, buyer *domain.User, now time.Time) {
	if buyer.ReferredBy == nil {
		return
	}
	referrer, err := a.users.GetByID(ctx, *buyer.ReferredBy)
	if err != nil {
		a.log.Error("payment apply: load referrer", "referrer_id", *buyer.ReferredBy, "err", err)
		return
	}

	plan, expiresAt, setPlan := domain.ApplyReferralReward(referrer, domain.ReferralPaidRewardDays, now)
	granted, err := a.referrals.GrantReward(ctx,
		referrer.ID, buyer.ID,
		domain.ReferralEventPaid, domain.ReferralPaidRewardDays, domain.ReferralYearlyCapDays,
		setPlan, plan, expiresAt)
	if err != nil {
		a.log.Error("payment apply: grant referral paid", "referrer_id", referrer.ID, "referee_id", buyer.ID, "err", err)
		return
	}
	if !granted {
		return // потолок за год или уже начисляли
	}
	a.log.Info("referral reward granted (paid)",
		"referrer_id", referrer.ID, "referee_id", buyer.ID, "days", domain.ReferralPaidRewardDays, "set_plan", setPlan)
	a.notify.ReferralPaid(ctx, referrer, buyer.Username, domain.ReferralPaidRewardDays, setPlan)
}
