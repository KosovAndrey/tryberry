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

// Service создаёт платежи из витрины тарифов через выбранный Provider.
type Service struct {
	provider  Provider
	payments  *postgres.PaymentRepo
	discounts *redisrepo.DiscountStore // nil → скидки не применяем (нет Redis)
	log       *slog.Logger
}

func NewService(provider Provider, payments *postgres.PaymentRepo, discounts *redisrepo.DiscountStore, log *slog.Logger) *Service {
	return &Service{provider: provider, payments: payments, discounts: discounts, log: log}
}

// Checkout — результат создания платежа для показа юзеру.
type Checkout struct {
	ConfirmationURL string
	AmountKopecks   int64
	DiscountPct     int // 0, если скидки не было
}

// Start создаёт разовый платёж за тариф plan для пользователя u и возвращает
// ссылку на оплату. Учитывает ожидающую скидку (если есть). Если email задан —
// провайдер приложит фискальный чек. План продлится на вебхуке после оплаты.
func (s *Service) Start(ctx context.Context, u *domain.User, plan, email string) (Checkout, error) {
	var out Checkout

	p, ok := domain.PlanByName(plan)
	if !ok || p.PriceRub <= 0 {
		return out, fmt.Errorf("plan %q is not purchasable", plan)
	}
	amount := int64(p.PriceRub) * 100

	// Ожидающая скидка (best-effort: ошибки Redis не валят оплату).
	var promoCodeID *int64
	if s.discounts != nil {
		if d, found, err := s.discounts.Get(ctx, u.ID); err != nil {
			s.log.Warn("checkout: read pending discount", "user_id", u.ID, "err", err)
		} else if found && d.Pct > 0 {
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
		Kind:           domain.PayKindOnetime,
		Plan:           plan,
		Days:           domain.PurchaseDays,
		AmountKopecks:  amount,
		PromoCodeID:    promoCodeID,
	})
	if err != nil {
		return out, fmt.Errorf("create payment row: %w", err)
	}

	description := fmt.Sprintf("Подписка TryberryBot — тариф %s, %d дней", p.Title, domain.PurchaseDays)
	res, err := s.provider.Checkout(ctx, CheckoutParams{
		PaymentID:     paymentID,
		UserID:        u.ID,
		Plan:          plan,
		Description:   description,
		AmountKopecks: amount,
		Email:         email,
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
