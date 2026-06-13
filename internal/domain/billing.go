package domain

import "time"

// Способы оплаты на витрине: разовый платёж или подписка с автопродлением.
const (
	PayKindOnetime    = "onetime"              // разовая оплата
	PayKindSubInitial = "subscription_initial" // первый платёж подписки (с Recurring)
	PayKindSubRenewal = "subscription_renewal" // автосписание по подписке
)

// Провайдеры оплаты (колонка payments.provider, флаг PAYMENT_PROVIDER).
const (
	ProviderYooKassa  = "yookassa"
	ProviderRobokassa = "robokassa"
)

// Статусы биллинговой подписки (billing_subscriptions.status).
const (
	SubStatusActive   = "active"   // автопродление включено
	SubStatusCanceled = "canceled" // отменена юзером, доступ до конца периода
	SubStatusPastDue  = "past_due" // списание не прошло, в ретраях dunning
	SubStatusExpired  = "expired"  // окончательно: ретраи исчерпаны
)

// SubChargeLeadTime — за сколько до истечения плана списываем автопродление,
// чтобы новый период начинался без разрыва доступа.
const SubChargeLeadTime = 1 * 24 * time.Hour

// SubMaxChargeFails — после скольких неудачных списаний подписка переходит из
// past_due в expired (доступ истекает по плану, просим продлить вручную).
const SubMaxChargeFails = 3

// BillingSubscription — рекуррентная подписка пользователя (строка таблицы
// billing_subscriptions). Не путать с domain.Subscription — та про товарные
// подписки на снижение цены.
type BillingSubscription struct {
	ID                 int64
	UserID             int64
	Plan               string
	Status             string
	AmountKopecks      int64 // сумма автопродления (sub-цена, без промо)
	RecurringInvoiceID int64 // PreviousInvoiceID = InvId первого платежа
	NextChargeAt       time.Time
	PreNoticeSentAt    *time.Time
	FailCount          int
	LastPaymentID      *int64
	CreatedAt          time.Time
	UpdatedAt          time.Time
	CanceledAt         *time.Time
}

// SubscriptionConsent — лог явного согласия на подписку (защита от чарджбэка и
// претензий по ЗоЗПП): кто, когда, на какую сумму и текст условий какой версии.
type SubscriptionConsent struct {
	ID            int64
	UserID        int64
	Plan          string
	AmountKopecks int64
	TermsVersion  string
	Platform      string // tg | vk
	CreatedAt     time.Time
}

// SubPriceKopecks — цена автопродления плана в копейках (0 → подписка недоступна).
func SubPriceKopecks(plan string) int64 {
	p, ok := PlanByName(plan)
	if !ok || p.SubPriceRub <= 0 {
		return 0
	}
	return int64(p.SubPriceRub) * 100
}

// PriceKopecks — цена разовой оплаты плана в копейках (0 → план не покупается).
func PriceKopecks(plan string) int64 {
	p, ok := PlanByName(plan)
	if !ok || p.PriceRub <= 0 {
		return 0
	}
	return int64(p.PriceRub) * 100
}
