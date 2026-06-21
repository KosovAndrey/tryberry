package domain

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// «Честная цена»: оценка текущей цены товара относительно НАШЕЙ истории наблюдений
// (price_history). Цель — отсечь фейковые скидки РФ-маркетплейсов: «−40%» от
// задранной планки. Сравниваем с минимумом/медианой за окна, а не с витринной
// зачёркнутой ценой (её скрейперы пока не отдают — см. docs/features/honest-price.md).

// PriceStats — агрегаты price_history по товару (заполняет PriceHistoryRepo.Stats).
// Min30/Median30 взвешены ПО ДЛИТЕЛЬНОСТИ сегментов (см. Stats): при change-only
// хранении точки неравномерны, поэтому «обычная цена» = медиана по времени, где
// товар провёл половину наблюдения, а не по числу записей.
type PriceStats struct {
	Min30, Median30 float64 // минимум и медиана за 30 дней (time-weighted)
	Min90           float64 // минимум за 90 дней
	MinAll          float64 // минимум за всё наблюдение
	Seg30           int     // сегментов цены, пересекающих 30-дн окно
	CountAll        int     // записей всего
	Since           time.Time
	HasData         bool // есть хоть одна запись
}

// honestMinAge — минимальный срок наблюдения, прежде чем делать выводы. При
// change-only хранении число записей НЕ показатель достаточности (стабильный товар
// может иметь 1 запись на 40 дней), поэтому гейтим по ВОЗРАСТУ наблюдения, а не по
// количеству точек. История есть только с момента, как МЫ начали трекать товар.
const honestMinAge = 7 * 24 * time.Hour

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

	tooYoung := s.Since.IsZero() || now.Sub(s.Since) < honestMinAge
	if current <= 0 || !s.HasData || tooYoung {
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

// TargetSuggestion — предложенная целевая цена для триггера below_target (кнопка в боте).
type TargetSuggestion struct {
	Price float64 // рубли, округлено
	Label string  // пояснение («минимум за 90 дней», «−10%»)
}

// SuggestTargets предлагает варианты целевой цены для «уведомить когда дешевле X»,
// опираясь на историю (honest-price): минимум за 90 дней и −5% от обычной (медианы).
// Всегда добавляет относительный фолбэк «−10% от текущей», чтобы кнопки были даже без
// истории. Возвращает только цены СТРОГО НИЖЕ current (target ≥ current сработал бы
// мгновенно), дедуплицирует близкие, сортирует по возрастанию, максимум 3.
func SuggestTargets(current float64, s PriceStats, now time.Time) []TargetSuggestion {
	if current <= 0 {
		return nil
	}
	type cand struct {
		price float64
		label string
	}
	var cands []cand
	// Из истории — только если данных достаточно (тот же гейт, что у вердикта).
	if AssessHonestPrice(current, s, now).Verdict != VerdictInsufficient {
		if s.Min90 > 0 {
			cands = append(cands, cand{math.Round(s.Min90), "минимум за 90 дней"})
		}
		if s.Median30 > 0 {
			cands = append(cands, cand{math.Round(s.Median30 * 0.95), "−5% от обычной"})
		}
	}
	cands = append(cands, cand{math.Round(current * 0.90), "−10%"}) // относительный фолбэк — всегда

	out := make([]TargetSuggestion, 0, len(cands))
	for _, c := range cands {
		if c.price <= 0 || c.price >= current {
			continue // target ≥ current бесполезен (сработал бы сразу)
		}
		dup := false
		for _, o := range out {
			if math.Abs(o.Price-c.price)/current < 0.01 { // близкие цены не дублируем
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, TargetSuggestion{Price: c.price, Label: c.label})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Price < out[j].Price })
	if len(out) > 3 {
		out = out[:3]
	}
	return out
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
