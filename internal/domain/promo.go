package domain

import (
	"errors"
	"strings"
	"time"
)

// Типы промокодов.
const (
	PromoKindGrant    = "grant"    // N дней тарифа бесплатно
	PromoKindDiscount = "discount" // скидка % к платежу (применяется при оплате)
)

var (
	// ErrPromoExhausted — лимит использований кода исчерпан.
	ErrPromoExhausted = errors.New("promo exhausted")

	// ErrPromoAlreadyRedeemed — пользователь уже погасил этот код.
	ErrPromoAlreadyRedeemed = errors.New("promo already redeemed")

	// ErrPromoPlanConflict — у пользователя активен другой платный план,
	// grant-код не применяем (никаких конвертаций дней между тарифами).
	ErrPromoPlanConflict = errors.New("promo plan conflict")
)

// PromoCode — промокод. Для kind=grant заполнены Plan и Days,
// для kind=discount — DiscountPct.
type PromoCode struct {
	ID          int64
	Code        string // всегда UPPER
	Kind        string
	Plan        string
	Days        int
	DiscountPct int
	MaxUses     int
	UsedCount   int
	ExpiresAt   *time.Time
	Active      bool
	CreatedAt   time.Time
}

// NormalizePromoCode — каноничная форма кода: трим + UPPER.
func NormalizePromoCode(code string) string {
	return strings.ToUpper(strings.TrimSpace(code))
}

// ApplyGrantPromo решает, что даёт grant-код пользователю, и возвращает срок
// действия нового плана. Правила (простые, без конвертаций):
//   - free или trial (или истёкший план) → выдаём план кода: now + days;
//   - тот же план активен → продлеваем: от текущего expires_at + days;
//   - другой платный план активен → ErrPromoPlanConflict;
//   - тот же план без срока (бессрочный, выдан админом) → ErrPromoPlanConflict,
//     продлевать бесконечность некуда.
func ApplyGrantPromo(u *User, p PromoCode, now time.Time) (time.Time, error) {
	cur := u.EffectivePlan(now)

	switch cur.Name {
	case planFree, "trial":
		return now.Add(time.Duration(p.Days) * 24 * time.Hour), nil
	case p.Plan:
		if u.PlanExpiresAt == nil {
			return time.Time{}, ErrPromoPlanConflict
		}
		return u.PlanExpiresAt.Add(time.Duration(p.Days) * 24 * time.Hour), nil
	default:
		return time.Time{}, ErrPromoPlanConflict
	}
}
