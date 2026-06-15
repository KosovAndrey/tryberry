package payment

import "context"

// Provider — платёжный шлюз за флагом PAYMENT_PROVIDER. Абстрагирует ЮKassa и
// Робокассу: Service создаёт строку payments и просит у провайдера ссылку на
// оплату; рекуррент (автосписание) умеет только провайдер, который его
// поддерживает (Робокасса) — остальные возвращают ErrRecurringUnsupported.
type Provider interface {
	// Name — имя провайдера (domain.ProviderYooKassa / ProviderRobokassa).
	Name() string

	// SupportsRecurring — умеет ли провайдер автосписания (подписку). Если нет,
	// витрина не предлагает подписку, только разовую оплату.
	SupportsRecurring() bool

	// Checkout — ссылка на оплату для уже созданной строки payments.
	Checkout(ctx context.Context, p CheckoutParams) (CheckoutResult, error)

	// ChargeRecurring — автосписание по сохранённой связке (PreviousInvoiceID).
	// Результат приходит асинхронно на вебхук провайдера, как и обычная оплата.
	ChargeRecurring(ctx context.Context, p RecurringParams) error
}

// CheckoutParams — данные для ссылки на оплату.
type CheckoutParams struct {
	PaymentID     int64  // наш payments.id (InvId для Робокассы, metadata для ЮKassa)
	UserID        int64  // для metadata/маршрутизации вебхука
	Plan          string // имя тарифа
	Description   string // описание для платёжной формы и чека
	AmountKopecks int64  // сумма к оплате (со скидкой)
	Email         string // email для чека (пусто → без чека на стороне провайдера)
	Recurring     bool   // первый платёж подписки (сохранить связку для автосписаний)
}

// CheckoutResult — результат создания платежа у провайдера.
type CheckoutResult struct {
	URL        string // ссылка/redirect на оплату
	ExternalID string // id платежа у провайдера (для Робокассы == strconv(PaymentID))
}

// RecurringParams — данные для автосписания по подписке.
type RecurringParams struct {
	PaymentID         int64 // новый payments.id (новый InvId)
	PreviousInvoiceID int64 // InvId первого платежа подписки
	Plan              string
	Description       string
	AmountKopecks     int64
	Email             string
}
