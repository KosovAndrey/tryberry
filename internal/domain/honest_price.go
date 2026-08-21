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
// var (не const) — чтобы можно было снизить для канареечного теста дайджеста
// (DIGEST_MIN_AGE_DAYS=0).
//
// 14 дней, а не 7 (поднято 21.08.2026). Семи дней хватает, чтобы вердикт был
// формально посчитан, но не хватает, чтобы он что-то значил: недельное окно
// накрывает один цикл распродаж маркетплейса, и «обычная цена» по нему — это
// цена одной акции. Повод — повторная сверка price-insight
// (docs/PRICE-INSIGHT-REVIEW.md §5.2): за неделю ≈1796 товаров перешагнули
// семидневный гейт и начали получать вердикты, не став информированнее.
var honestMinAge = 14 * 24 * time.Hour

// medianWindow — номинальное окно медианы/минимума, которое называют тексты
// вердиктов. Если наблюдение короче, Line подставляет фактический срок: сказать
// «медиана за 30 дней», посчитав её по 17 дням, — это заявить горизонт, которого
// нет.
//
// Минимумы за 30/90 дней такой правки НЕ требуют: Min30 >= Min90 >= MinAll по
// построению (шире окно — меньше минимум), поэтому ветка VerdictLowest90
// достижима только когда MinAll < Min90, то есть когда есть точка старше 90
// дней; VerdictLowest30 — только когда наблюдение длиннее 30 дней. На коротком
// окне обе ветки структурно недостижимы (подтверждено на проде: в матрице
// вердиктов price-insight с окном 17 дней нет ни одного 2 или 3).
const medianWindow = 30 * 24 * time.Hour

// SetHonestMinAge переопределяет порог достаточности (для теста). 0 → выводы сразу.
func SetHonestMinAge(d time.Duration) { honestMinAge = d }

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
	// Observed — фактический срок наблюдения на момент оценки (now - Since).
	// Нужен рендеру: тексты называют окно, и называть его надо честное.
	Observed time.Duration
}

// AssessHonestPrice классифицирует текущую цену относительно истории. current уже
// записан в price_history к моменту оценки, поэтому он входит в min/median (current
// == MinAll означает «минимум за всё время»). Медиану берём (а не среднее) — она
// устойчива к выбросам и к попытке «накрутить» нашу же историю одним скачком.
func AssessHonestPrice(current float64, s PriceStats, now time.Time) HonestPrice {
	hp := HonestPrice{Min30: s.Min30, Median30: s.Median30, Min90: s.Min90, MinAll: s.MinAll}
	if !s.Since.IsZero() {
		hp.Observed = now.Sub(s.Since)
	}

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
		return fmt.Sprintf("🔴 Выше обычной — медиана за %s %.0f ₽", hp.medianWindowLabel(), hp.Median30)
	default:
		return ""
	}
}

// medianWindowLabel — как назвать окно медианы в тексте. Пока наблюдение короче
// номинальных 30 дней, называем фактический срок: медиана посчитана по тому, что
// есть, и выдавать её за тридцатидневную значит соврать про горизонт.
func (hp HonestPrice) medianWindowLabel() string {
	if hp.Observed <= 0 || hp.Observed >= medianWindow {
		return "30 дней"
	}
	days := int(hp.Observed / (24 * time.Hour))
	if days < 1 {
		days = 1
	}
	return fmt.Sprintf("%d %s наблюдения", days, pluralDays(days))
}

// pluralDays — русское склонение слова «день» для чисел 1..29 (шире не нужно:
// метка используется только когда наблюдение короче 30 дней).
func pluralDays(n int) string {
	if n%100 >= 11 && n%100 <= 14 {
		return "дней"
	}
	switch n % 10 {
	case 1:
		return "день"
	case 2, 3, 4:
		return "дня"
	default:
		return "дней"
	}
}
