package domain

import (
	"fmt"
	"time"
)

// «Честная цена»: оценка текущей цены товара относительно НАШЕЙ истории наблюдений
// (price_history). Цель — отсечь фейковые скидки РФ-маркетплейсов: «−40%» от
// задранной планки. Сравниваем с минимумом/медианой за окна, а не с витринной
// зачёркнутой ценой (её скрейперы пока не отдают — см. docs/features/honest-price.md).

// PriceStats — агрегаты price_history по товару (заполняет PriceHistoryRepo.Stats).
type PriceStats struct {
	Min30, Median30 float64 // минимум и медиана за 30 дней
	Min90           float64 // минимум за 90 дней
	MinAll          float64 // минимум за всё наблюдение
	Count30         int     // точек за 30 дней
	CountAll        int     // точек всего
	Since           time.Time
	HasData         bool // есть хоть одна запись
}

// Пороги «достаточности данных»: история есть только с момента, как МЫ начали
// трекать товар, поэтому на малой выборке выводы не делаем (честность важнее бейджа).
const (
	honestMinSamples = 5
	honestMinAge     = 3 * 24 * time.Hour
)

type PriceVerdict int

const (
	VerdictInsufficient PriceVerdict = iota // данных мало — наблюдаем
	VerdictLowestEver                       // ≤ минимума за всё наблюдение
	VerdictLowest90                         // ≤ минимума за 90 дней
	VerdictLowest30                         // ≤ минимума за 30 дней
	VerdictTypical                          // ≤ медианы за 30 дней — обычная цена
	VerdictAboveTypical                     // выше медианы за 30 дней — «скидка» завышена
)

// HonestPrice — вердикт + опорные числа (для рендера в TG/VK).
type HonestPrice struct {
	Verdict                        PriceVerdict
	Min30, Median30, Min90, MinAll float64
}

// AssessHonestPrice классифицирует текущую цену относительно истории. current уже
// записан в price_history к моменту оценки, поэтому он входит в min/median (current
// == MinAll означает «минимум за всё время»). Медиану берём (а не среднее) — она
// устойчива к выбросам и к попытке «накрутить» нашу же историю одним скачком.
func AssessHonestPrice(current float64, s PriceStats, now time.Time) HonestPrice {
	hp := HonestPrice{Min30: s.Min30, Median30: s.Median30, Min90: s.Min90, MinAll: s.MinAll}

	tooYoung := !s.Since.IsZero() && now.Sub(s.Since) < honestMinAge
	if current <= 0 || !s.HasData || s.Count30 < honestMinSamples || tooYoung {
		hp.Verdict = VerdictInsufficient
		return hp
	}

	switch {
	case s.MinAll > 0 && current <= s.MinAll:
		hp.Verdict = VerdictLowestEver
	case s.Min90 > 0 && current <= s.Min90:
		hp.Verdict = VerdictLowest90
	case s.Min30 > 0 && current <= s.Min30:
		hp.Verdict = VerdictLowest30
	case s.Median30 > 0 && current <= s.Median30:
		hp.Verdict = VerdictTypical
	default:
		hp.Verdict = VerdictAboveTypical
	}
	return hp
}

// Line — одна строка для вставки в уведомление (TG и VK). Пусто для Insufficient
// (ничего не добавляем, чтобы не вводить в заблуждение на малой выборке).
func (hp HonestPrice) Line() string {
	switch hp.Verdict {
	case VerdictLowestEver:
		return "🟢 Минимальная цена за всё время наблюдения"
	case VerdictLowest90:
		return "🟢 Минимум за 90 дней"
	case VerdictLowest30:
		return "🟢 Минимум за 30 дней"
	case VerdictTypical:
		return "🟡 Обычная цена для этого товара"
	case VerdictAboveTypical:
		return fmt.Sprintf("🔴 Выше обычной — медиана за 30 дней %.0f ₽", hp.Median30)
	default:
		return ""
	}
}
