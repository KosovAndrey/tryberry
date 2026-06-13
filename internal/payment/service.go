// Package payment связывает витрину тарифов (TG/VK) с ЮKassa: создаёт платёж,
// применяет ожидающую скидку и отдаёт ссылку на оплату. Применение оплаты
// (продление плана) живёт в консьюмере bot-worker — см. PaymentRepo.MarkSucceeded.
package payment

import (
	"context"
	"fmt"
	"log/slog"
	"strconv"

	"github.com/google/uuid"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/yookassa"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	redisrepo "gitlab.com/KosovAndrey/tryberrybot/internal/repository/redis"
)

// Service создаёт платежи ЮKassa из витрины тарифов.
type Service struct {
	yk        *yookassa.Client
	payments  *postgres.PaymentRepo
	discounts *redisrepo.DiscountStore // nil → скидки не применяем (нет Redis)
	returnURL string
	vatCode   int // код ставки НДС в чеке (самозанятый → 1 = «без НДС»)
	log       *slog.Logger
}

func NewService(yk *yookassa.Client, payments *postgres.PaymentRepo, discounts *redisrepo.DiscountStore, returnURL string, vatCode int, log *slog.Logger) *Service {
	return &Service{yk: yk, payments: payments, discounts: discounts, returnURL: returnURL, vatCode: vatCode, log: log}
}

// Checkout — результат создания платежа для показа юзеру.
type Checkout struct {
	ConfirmationURL string
	AmountKopecks   int64
	DiscountPct     int // 0, если скидки не было
}

// Start создаёт платёж за тариф plan для пользователя u и возвращает ссылку на
// оплату. Учитывает ожидающую скидку (если есть). Если email задан — прикладывает
// фискальный чек 54-ФЗ (ЮKassa отправит чек на этот адрес). Идемпотентность
// create — через Idempotence-Key; план продлится на вебхуке после оплаты.
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
		Plan:           plan,
		Days:           domain.PurchaseDays,
		AmountKopecks:  amount,
		PromoCodeID:    promoCodeID,
	})
	if err != nil {
		return out, fmt.Errorf("create payment row: %w", err)
	}

	description := fmt.Sprintf("Подписка TryberryBot — тариф %s, %d дней", p.Title, domain.PurchaseDays)
	req := yookassa.CreateRequest{
		Amount:       yookassa.Amount{Value: domain.KopecksToRubString(amount), Currency: "RUB"},
		Capture:      true,
		Confirmation: yookassa.Confirmation{Type: "redirect", ReturnURL: s.returnURL},
		Description:  description,
		Metadata: map[string]string{
			"payment_id": strconv.FormatInt(paymentID, 10),
			"user_id":    strconv.FormatInt(u.ID, 10),
			"plan":       plan,
		},
	}
	// Чек 54-ФЗ: одна позиция-услуга на всю сумму, ставка НДС из конфига
	// (самозанятый → 1 = «без НДС»), полная предоплата.
	if email != "" {
		req.Receipt = &yookassa.Receipt{
			Customer: yookassa.ReceiptCustomer{Email: email},
			Items: []yookassa.ReceiptItem{{
				Description:    description,
				Quantity:       "1.00",
				Amount:         yookassa.Amount{Value: domain.KopecksToRubString(amount), Currency: "RUB"},
				VATCode:        s.vatCode,
				PaymentSubject: "service",
				PaymentMode:    "full_prepayment",
			}},
		}
	}
	resp, err := s.yk.CreatePayment(ctx, idemKey, req)
	if err != nil {
		return out, fmt.Errorf("yookassa create: %w", err)
	}
	if err := s.payments.SetYKID(ctx, paymentID, resp.ID); err != nil {
		// Платёж в ЮKassa создан — не теряем связь, но и не падаем для юзера.
		s.log.Error("checkout: set yk_payment_id", "payment_id", paymentID, "yk_id", resp.ID, "err", err)
	}
	if resp.Confirmation.ConfirmationURL == "" {
		return out, fmt.Errorf("yookassa: empty confirmation_url for payment %s", resp.ID)
	}
	out.ConfirmationURL = resp.Confirmation.ConfirmationURL
	return out, nil
}
