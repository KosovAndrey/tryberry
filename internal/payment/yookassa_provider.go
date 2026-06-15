package payment

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/google/uuid"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/yookassa"
)

// ErrRecurringUnsupported — провайдер не умеет автосписания (ЮKassa в нашей
// интеграции). Подписку на таком провайдере не предлагаем.
var ErrRecurringUnsupported = errors.New("payment: recurring not supported by provider")

// ykProvider — адаптер ЮKassa под интерфейс Provider. Поведение прежнее:
// create через Idempotence-Key, чек 54-ФЗ одной позицией-услугой.
type ykProvider struct {
	yk        *yookassa.Client
	returnURL string
	vatCode   int // код ставки НДС (самозанятый → 1 = «без НДС»)
}

// NewYooKassaProvider — провайдер ЮKassa.
func NewYooKassaProvider(yk *yookassa.Client, returnURL string, vatCode int) Provider {
	return &ykProvider{yk: yk, returnURL: returnURL, vatCode: vatCode}
}

func (p *ykProvider) Name() string { return domain.ProviderYooKassa }

func (p *ykProvider) SupportsRecurring() bool { return false }

func (p *ykProvider) Checkout(ctx context.Context, c CheckoutParams) (CheckoutResult, error) {
	var out CheckoutResult

	idemKey := uuid.NewString()
	req := yookassa.CreateRequest{
		Amount:       yookassa.Amount{Value: domain.KopecksToRubString(c.AmountKopecks), Currency: "RUB"},
		Capture:      true,
		Confirmation: yookassa.Confirmation{Type: "redirect", ReturnURL: p.returnURL},
		Description:  c.Description,
		Metadata: map[string]string{
			"payment_id": strconv.FormatInt(c.PaymentID, 10),
			"user_id":    strconv.FormatInt(c.UserID, 10),
			"plan":       c.Plan,
		},
	}
	// Чек 54-ФЗ: одна позиция-услуга на всю сумму, ставка НДС из конфига
	// (самозанятый → 1 = «без НДС»), полная предоплата.
	if c.Email != "" {
		req.Receipt = &yookassa.Receipt{
			Customer: yookassa.ReceiptCustomer{Email: c.Email},
			Items: []yookassa.ReceiptItem{{
				Description:    c.Description,
				Quantity:       "1.00",
				Amount:         yookassa.Amount{Value: domain.KopecksToRubString(c.AmountKopecks), Currency: "RUB"},
				VATCode:        p.vatCode,
				PaymentSubject: "service",
				PaymentMode:    "full_prepayment",
			}},
		}
	}

	resp, err := p.yk.CreatePayment(ctx, idemKey, req)
	if err != nil {
		return out, fmt.Errorf("yookassa create: %w", err)
	}
	if resp.Confirmation.ConfirmationURL == "" {
		return out, fmt.Errorf("yookassa: empty confirmation_url for payment %s", resp.ID)
	}
	out.URL = resp.Confirmation.ConfirmationURL
	out.ExternalID = resp.ID
	return out, nil
}

// ChargeRecurring — ЮKassa в нашей интеграции автосписания не делает.
func (p *ykProvider) ChargeRecurring(ctx context.Context, _ RecurringParams) error {
	return ErrRecurringUnsupported
}
