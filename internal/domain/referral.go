package domain

import "time"

// Реферальные события (referral_rewards.event).
const (
	ReferralEventActivated = "activated" // друг прожил 48ч и держит активную подписку
	ReferralEventPaid      = "paid"      // друг оплатил тариф (ЮKassa)
)

// ЦИФРЫ РЕФЕРАЛКИ МЕНЯЮТСЯ ЗДЕСЬ (как и каталог тарифов).
const (
	// ReferralTrialDuration — расширенный триал приглашённому (вместо TrialDuration).
	ReferralTrialDuration = 7 * 24 * time.Hour

	// ReferralActivatedRewardDays — дней рефереру за «активного» друга.
	ReferralActivatedRewardDays = 5

	// ReferralPaidRewardDays — дней рефереру за первую оплату друга.
	ReferralPaidRewardDays = 30

	// ReferralActivationAge — сколько друг должен прожить с активной подпиской,
	// прежде чем реферер получит награду (анти-фарм мультиаккаунтами).
	ReferralActivationAge = 48 * time.Hour

	// ReferralYearlyCapDays — потолок начислений рефереру за скользящий год.
	ReferralYearlyCapDays = 90

	// ReferralAttributionWindow — окно после создания аккаунта, в котором
	// /start ref_XXX ещё засчитывает атрибуцию. Старые аккаунты, кликнувшие
	// чужую ссылку, рефералами не становятся.
	ReferralAttributionWindow = 10 * time.Minute

	// referralRewardPlan — тариф, который получает реферер на free/триале.
	referralRewardPlan = "lite"
)

// ApplyReferralReward решает, что даёт награда в N дней рефереру:
//   - free/trial (или истёкший план) → lite на N дней от сейчас;
//   - платный план со сроком → +N дней к текущему сроку (план тот же);
//   - план без срока (бессрочный от админа) → продлевать некуда, ok=false —
//     награда только записывается в аудит.
func ApplyReferralReward(u *User, days int, now time.Time) (plan string, expiresAt time.Time, ok bool) {
	cur := u.EffectivePlan(now)
	d := time.Duration(days) * 24 * time.Hour

	switch {
	case cur.Name == planFree || cur.Name == "trial":
		return referralRewardPlan, now.Add(d), true
	case u.PlanExpiresAt == nil:
		return "", time.Time{}, false
	default:
		return cur.Name, u.PlanExpiresAt.Add(d), true
	}
}
