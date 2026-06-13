package payment

import (
	"context"
	"fmt"
	"strconv"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/payment/robokassa"
)

// rkProvider — адаптер Робокассы под интерфейс Provider. Внешний id платежа ==
// наш payments.id (InvId числовой). Чек НПД прикладываем, если включена
// фискализация (режим самозанятого), — на каждую оплату и автосписание.
type rkProvider struct {
	rk     *robokassa.Client
	fiscal bool // ROBOKASSA_NPD: формировать чек НПД
}

// NewRobokassaProvider — провайдер Робокассы.
func NewRobokassaProvider(rk *robokassa.Client, fiscal bool) Provider {
	return &rkProvider{rk: rk, fiscal: fiscal}
}

func (p *rkProvider) Name() string { return domain.ProviderRobokassa }

func (p *rkProvider) Checkout(ctx context.Context, c CheckoutParams) (CheckoutResult, error) {
	url, err := p.rk.BuildPaymentURL(robokassa.PaymentParams{
		InvID:       c.PaymentID,
		OutSum:      domain.KopecksToRubString(c.AmountKopecks),
		Description: c.Description,
		Email:       c.Email,
		Recurring:   c.Recurring,
		Receipt:     p.receipt(c.Description, c.AmountKopecks),
	})
	if err != nil {
		return CheckoutResult{}, fmt.Errorf("robokassa checkout: %w", err)
	}
	return CheckoutResult{URL: url, ExternalID: strconv.FormatInt(c.PaymentID, 10)}, nil
}

func (p *rkProvider) ChargeRecurring(ctx context.Context, r RecurringParams) error {
	return p.rk.ChargeRecurring(ctx, robokassa.RecurringParams{
		InvID:         r.PaymentID,
		PreviousInvID: r.PreviousInvoiceID,
		OutSum:        domain.KopecksToRubString(r.AmountKopecks),
		Description:   r.Description,
		Receipt:       p.receipt(r.Description, r.AmountKopecks),
	})
}

// receipt — чек НПД одной позицией-услугой на всю сумму. nil, если фискализация
// выключена (sno проставит клиент Робокассы).
func (p *rkProvider) receipt(description string, amountKopecks int64) *robokassa.Receipt {
	if !p.fiscal {
		return nil
	}
	return &robokassa.Receipt{
		Items: []robokassa.ReceiptItem{{
			Name:          description,
			Quantity:      1,
			Sum:           float64(amountKopecks) / 100,
			PaymentMethod: "full_prepayment",
			PaymentObject: "service",
			Tax:           "none", // самозанятый — без НДС
		}},
	}
}
