// Package payment связывает витрину тарифов (TG/VK) с платёжным провайдером
// (ЮKassa/Робокасса за флагом PAYMENT_PROVIDER): создаёт платёж, применяет
// ожидающую скидку и отдаёт ссылку на оплату. Применение оплаты (продление
// плана) живёт в консьюмере bot-worker — см. PaymentRepo.MarkSucceeded.
package payment

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/google/uuid"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
)

// ConsentLogger пишет факт согласия на подписку (реализует BillingSubscriptionRepo).
type ConsentLogger interface {
	LogConsent(ctx context.Context, c domain.SubscriptionConsent) error
}

// Service создаёт платежи из витрины тарифов через выбранный Provider.
type Service struct {
	provider  Provider
	payments  paymentCreator
	promos    promoCapacityChecker // nil → гейт скидки пропускаем (старое поведение)
	discounts discountReader       // nil → скидки не применяем (нет Redis)
	consents  ConsentLogger        // лог согласия на подписку; nil → не логируем
	log       *slog.Logger
}

func NewService(provider Provider, payments *postgres.PaymentRepo, promos *postgres.PromoRepo, discounts *redisrepo.DiscountStore, consents ConsentLogger, log *slog.Logger) *Service {
	s := &Service{provider: provider, payments: payments, consents: consents, log: log}
	// Только реально не-nil зависимости (typed-nil в интерфейсе != nil — сломал бы
	// nil-проверки на promos/discounts/consents).
	if promos != nil {
		s.promos = promos
	}
	if discounts != nil {
		s.discounts = discounts
	}
	return s
}

// SupportsSubscription — умеет ли текущий провайдер автосписания (для витрины).
func (s *Service) SupportsSubscription() bool { return s.provider.SupportsRecurring() }

// Checkout — результат создания платежа для показа юзеру.
type Checkout struct {
	ConfirmationURL string
	AmountKopecks   int64 // сумма первого платежа (со скидкой, если была)
	DiscountPct     int   // 0, если скидки не было
	Recurring       bool  // платёж — первый в подписке (дальше автосписания)
	RenewalKopecks  int64 // сумма автопродления (без промо), если Recurring
}

// Start создаёт разовый платёж за тариф plan и возвращает ссылку на оплату.
func (s *Service) Start(ctx context.Context, u *domain.User, plan, email string) (Checkout, error) {
	base := domain.PriceKopecks(plan)
	if base <= 0 {
		return Checkout{}, fmt.Errorf("plan %q is not purchasable", plan)
	}
	return s.checkout(ctx, u, plan, email, domain.PayKindOnetime, base, false, 0)
}

// StartSubscription создаёт первый платёж подписки (с автопродлением). Перед
// оплатой фиксирует согласие (платформа platform: tg|vk). Промо применяется
// только к первому платежу; автопродления идут по подписочной цене.
func (s *Service) StartSubscription(ctx context.Context, u *domain.User, plan, email, platform string) (Checkout, error) {
	if !s.provider.SupportsRecurring() {
		return Checkout{}, fmt.Errorf("provider %s: subscription not supported", s.provider.Name())
	}
	sub := domain.SubPriceKopecks(plan)
	if sub <= 0 {
		return Checkout{}, fmt.Errorf("plan %q has no subscription price", plan)
	}

	// Лог согласия на сумму автопродления (то, на что соглашается юзер регулярно).
	if s.consents != nil {
		if err := s.consents.LogConsent(ctx, domain.SubscriptionConsent{
			UserID: u.ID, Plan: plan, AmountKopecks: sub,
			TermsVersion: domain.SubTermsVersion, Platform: platform,
		}); err != nil {
			// Согласие — наша защита от чарджбэка; без записи оплату не начинаем.
			return Checkout{}, fmt.Errorf("log consent: %w", err)
		}
	}
	return s.checkout(ctx, u, plan, email, domain.PayKindSubInitial, sub, true, sub)
}

// checkout — общий путь создания платежа: применяет ожидающую скидку к первому
// платежу, заводит строку payments и берёт у провайдера ссылку на оплату.
func (s *Service) checkout(ctx context.Context, u *domain.User, plan, email, kind string, baseAmount int64, recurring bool, renewalKopecks int64) (Checkout, error) {
	out := Checkout{Recurring: recurring, RenewalKopecks: renewalKopecks}
	amount := baseAmount

	// Ожидающая скидка (best-effort: ошибки Redis не валят оплату).
	var promoCodeID *int64
	if s.discounts != nil {
		if d, found, err := s.discounts.Get(ctx, u.ID); err != nil {
			s.log.Warn("checkout: read pending discount", "user_id", u.ID, "err", err)
		} else if found && d.Pct > 0 && s.discountUsable(ctx, d.CodeID, u.ID) {
			amount = domain.DiscountedKopecks(amount, d.Pct)
			id := d.CodeID
			promoCodeID = &id
			out.DiscountPct = d.Pct
		}
	}
	out.AmountKopecks = amount

	idemKey := uuid.NewString()
	paymentID, err := s.payments.Create(ctx, domain.Payment{
		UserID:         u.ID,
		IdempotenceKey: idemKey,
		Provider:       s.provider.Name(),
		Kind:           kind,
		Plan:           plan,
		Days:           domain.PurchaseDays,
		AmountKopecks:  amount,
		PromoCodeID:    promoCodeID,
	})
	if err != nil {
		return out, fmt.Errorf("create payment row: %w", err)
	}

	p, _ := domain.PlanByName(plan)
	description := fmt.Sprintf("Подписка TryberryBot — тариф %s, %d дней", p.Title, domain.PurchaseDays)
	res, err := s.provider.Checkout(ctx, CheckoutParams{
		PaymentID:     paymentID,
		UserID:        u.ID,
		Plan:          plan,
		Description:   description,
		AmountKopecks: amount,
		Email:         email,
		Recurring:     recurring,
	})
	if err != nil {
		return out, fmt.Errorf("provider checkout: %w", err)
	}
	if err := s.payments.SetYKID(ctx, paymentID, res.ExternalID); err != nil {
		// Платёж у провайдера создан — не теряем связь, но и не падаем для юзера.
		s.log.Error("checkout: set external payment id", "payment_id", paymentID, "external_id", res.ExternalID, "err", err)
	}
	out.ConfirmationURL = res.URL
	return out, nil
}

// discountUsable — пригоден ли discount-код к применению прямо сейчас (есть
// свободные активации). Гонка не страшна: между этой проверкой и гашением после
// оплаты код может исчерпаться — окончательный лимит держит RedeemDiscount, а
// здесь мы лишь не показываем скидку по очевидно исчерпанному коду. Нет чекера
// или сбой запроса → считаем пригодным (best-effort, не валим оплату из-за БД).
func (s *Service) discountUsable(ctx context.Context, codeID, userID int64) bool {
	if s.promos == nil {
		return true
	}
	ok, err := s.promos.Redeemable(ctx, codeID, userID)
	if err != nil {
		s.log.Warn("checkout: check discount capacity", "code_id", codeID, "err", err)
		return true
	}
	if !ok {
		s.log.Info("checkout: discount code exhausted or already redeemed, skipping", "code_id", codeID, "user_id", userID)
	}
	return ok
}
