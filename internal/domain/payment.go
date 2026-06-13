package domain

import (
	"fmt"
	"net/mail"
	"strings"
	"time"
)

// ValidEmail — нормализованный email и ok=true, если адрес валиден. Нужен для
// чека 54-ФЗ (ЮKassa шлёт чек на него).
func ValidEmail(s string) (string, bool) {
	addr, err := mail.ParseAddress(strings.TrimSpace(s))
	if err != nil {
		return "", false
	}
	return addr.Address, true
}

// PurchaseDays — срок подписки за одну оплату (витрина обещает «30 дней»).
const PurchaseDays = 30

// Payment — платёж ЮKassa (строка таблицы payments).
type Payment struct {
	ID             int64
	UserID         int64
	YKPaymentID    string
	IdempotenceKey string
	Plan           string
	Days           int
	AmountKopecks  int64
	PromoCodeID    *int64 // discount-код, если применён
	Status         string // pending | succeeded | canceled
	CreatedAt      time.Time
	PaidAt         *time.Time
}

// ApplyPurchase — новый срок действия плана после оплаты days дней тарифа plan.
// Покупка всегда проходит (в отличие от grant-промокода с ErrPromoPlanConflict):
// тот же активный план продлеваем от текущего срока, иначе ставим now+days.
func ApplyPurchase(u *User, plan string, days int, now time.Time) time.Time {
	d := time.Duration(days) * 24 * time.Hour
	cur := u.EffectivePlan(now)
	if cur.Name == plan && u.PlanExpiresAt != nil && now.Before(*u.PlanExpiresAt) {
		return u.PlanExpiresAt.Add(d)
	}
	return now.Add(d)
}

// DiscountedKopecks — цена со скидкой pct% (округление вниз, в пользу юзера).
func DiscountedKopecks(kopecks int64, pct int) int64 {
	if pct <= 0 {
		return kopecks
	}
	if pct >= 100 {
		return 0
	}
	return kopecks * int64(100-pct) / 100
}

// KopecksToRubString — копейки → "199.00" для поля amount.value ЮKassa.
func KopecksToRubString(kopecks int64) string {
	return fmt.Sprintf("%d.%02d", kopecks/100, kopecks%100)
}
