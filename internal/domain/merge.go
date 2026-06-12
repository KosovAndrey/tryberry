package domain

import (
	"fmt"
	"math"
	"time"
)

// Слияние двух непустых аккаунтов при привязке (оба имеют подписки/тариф).
// Подписки объединяются всегда; вопрос только в том, какой тариф остаётся.
//
// Правила:
//   - free + free          → free.
//   - trial + (free|trial) → trial с бОльшим остатком дней.
//   - paid  + (free|trial) → paid как есть (триал сгорает).
//   - paid  + paid, один план → тот же план, остатки дней складываются.
//   - paid  + paid, разные   → выбор пользователя из двух вариантов: оставить
//     любой из планов, остаток второго конвертируется по соотношению цен
//     (день дорогого = price_дорогого/price_дешёвого дней дешёвого), итог
//     округляется вверх.
//   - бессрочные платные (unlimited, ручные гранты без срока) несравнимы по
//     дням: выживает более «сильный» (бессрочный > срочного, при двух
//     бессрочных — дороже), без выбора.
//
// trial_used объединённого аккаунта = OR (плюс вечные trial_claims по
// идентичностям — фарм триалов через отвязку не работает).

// MergeOption — один из вариантов итогового тарифа.
type MergeOption struct {
	Plan      string     `json:"plan"`
	ExpiresAt *time.Time `json:"expires_at,omitempty"` // nil — бессрочно (free/unlimited)
	Days      int        `json:"days"`                 // остаток дней для отображения (0 — бессрочно/free)
}

// MergeDecision — результат ComputeMerge: один авто-вариант или выбор из двух
// (Options[0] — более дорогой план).
type MergeDecision struct {
	NeedChoice bool          `json:"need_choice"`
	Options    []MergeOption `json:"options"`
}

// MergePending — состояние диалога слияния (Redis, между нажатиями кнопок).
type MergePending struct {
	KeptID     int64         `json:"k"` // аккаунт-владелец кода (выживает как строка)
	AbsorbedID int64         `json:"a"` // аккаунт инициатора (контент переезжает)
	Decision   MergeDecision `json:"d"`
}

// MergeOptionLabel — «Pro до 17.07.2026 (35 дней)» / «Free» / «Unlimited (бессрочно)».
func MergeOptionLabel(o MergeOption) string {
	title := o.Plan
	if p, ok := PlanByName(o.Plan); ok {
		title = p.Title
	}
	if o.ExpiresAt == nil {
		if o.Plan == planFree {
			return title
		}
		return title + " (бессрочно)"
	}
	return fmt.Sprintf("%s до %s (%d %s)", title, o.ExpiresAt.Format("02.01.2006"), o.Days, DaysWord(o.Days))
}

// remainingDays — остаток оплаченных дней (дробный), 0 если срока нет.
func remainingDays(u *User, now time.Time) float64 {
	if u.PlanExpiresAt == nil || u.PlanExpired(now) {
		return 0
	}
	return u.PlanExpiresAt.Sub(now).Hours() / 24
}

// planRank — free=0, trial=1, платный=2.
func planRank(p Plan) int {
	switch {
	case p.Name == planFree:
		return 0
	case p.Name == "trial":
		return 1
	default:
		return 2
	}
}

func optionFor(plan Plan, days float64, now time.Time) MergeOption {
	d := int(math.Ceil(days))
	exp := now.Add(time.Duration(d) * 24 * time.Hour)
	return MergeOption{Plan: plan.Name, ExpiresAt: &exp, Days: d}
}

// ComputeMerge — какой тариф у объединённого аккаунта. a, b — два аккаунта
// (порядок не важен, опции при выборе сортируются по цене).
func ComputeMerge(a, b *User, now time.Time) MergeDecision {
	pa, pb := a.EffectivePlan(now), b.EffectivePlan(now)
	ra, rb := remainingDays(a, now), remainingDays(b, now)

	// Разный ранг → выживает старший как есть.
	if planRank(pa) != planRank(pb) {
		win, rem := pa, ra
		winU := a
		if planRank(pb) > planRank(pa) {
			win, rem, winU = pb, rb, b
		}
		if win.Name == planFree {
			return MergeDecision{Options: []MergeOption{{Plan: planFree}}}
		}
		if winU.PlanExpiresAt == nil {
			return MergeDecision{Options: []MergeOption{{Plan: win.Name}}}
		}
		return MergeDecision{Options: []MergeOption{optionFor(win, rem, now)}}
	}

	switch planRank(pa) {
	case 0: // free + free
		return MergeDecision{Options: []MergeOption{{Plan: planFree}}}

	case 1: // trial + trial → больший остаток
		rem := math.Max(ra, rb)
		return MergeDecision{Options: []MergeOption{optionFor(pa, rem, now)}}
	}

	// paid + paid.
	aInf := a.PlanExpiresAt == nil
	bInf := b.PlanExpiresAt == nil
	if aInf || bInf {
		// Бессрочные несравнимы по дням — выживает сильнейший без выбора.
		win := pa
		switch {
		case aInf && bInf:
			if pb.PriceRub > pa.PriceRub || pb.Name == "unlimited" {
				win = pb
			}
		case bInf:
			win = pb
		}
		return MergeDecision{Options: []MergeOption{{Plan: win.Name}}}
	}

	if pa.Name == pb.Name {
		return MergeDecision{Options: []MergeOption{optionFor(pa, ra+rb, now)}}
	}

	// Разные платные планы → выбор. exp/chp — дорогой/дешёвый.
	exp, rexp, chp, rchp := pa, ra, pb, rb
	if pb.PriceRub > pa.PriceRub {
		exp, rexp, chp, rchp = pb, rb, pa, ra
	}
	if chp.PriceRub <= 0 {
		// Цена 0 у legacy-плана — конвертировать нечем, оставляем дорогой.
		return MergeDecision{Options: []MergeOption{optionFor(exp, rexp, now)}}
	}
	ratio := float64(chp.PriceRub) / float64(exp.PriceRub)
	return MergeDecision{
		NeedChoice: true,
		Options: []MergeOption{
			optionFor(exp, rexp+rchp*ratio, now), // дни дешёвого → дни дорогого
			optionFor(chp, rchp+rexp/ratio, now), // дни дорогого → дни дешёвого
		},
	}
}
