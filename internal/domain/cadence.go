package domain

import "time"

// Волатильностный каданс: чем дольше цена товара/выдачи непрерывно не меняется,
// тем реже её опрашиваем. Бэкофф накопительный (растёт с ДЛИТЕЛЬНОСТЬЮ
// стабильности, а не после одной проверки без изменений) и мгновенно
// сбрасывается первым же изменением цены — колонка last_price_change_at /
// last_change_at обновляется воркером в момент фиксации изменения.
// Уведомление при бэкоффе не теряется, а лишь задерживается: алерт срабатывает
// на следующем скрейпе, когда бы он ни случился.
// Дизайн и худшие случаи: docs/TARIFF-FREE-SEARCH-LINK.md §2.

const (
	// VolatilityPopularSubs — со скольких активных подписчиков товар/запрос
	// считается популярным: один скрейп кормит многих, свежесть важнее
	// экономии, бэкофф капится VolatilityPopularCap.
	VolatilityPopularSubs = 5
	// VolatilityPopularCap — потолок множителя для популярных товаров/запросов.
	VolatilityPopularCap = 2.0

	// SearchIntervalCeil — абсолютный потолок интервала опроса ПОИСКА: любая
	// выдача проверяется минимум дважды в сутки даже после максимального
	// бэкоффа (фри-поиск 6ч не должен уезжать дальше 12ч). Товарам потолок не
	// нужен: их худший случай 60м×3 = 3ч.
	SearchIntervalCeil = 12 * time.Hour
)

// Лестница бэкоффа: длительность непрерывной стабильности цены → множитель.
const (
	volatilityTier1 = 5 * 24 * time.Hour  // младше — полный темп плана (×1)
	volatilityTier2 = 10 * 24 * time.Hour // 5–10 дней — ×1.5
	volatilityTier3 = 20 * 24 * time.Hour // 10–20 дней — ×2, старше — ×3
)

// VolatilityMult — множитель к эффективному интервалу опроса товара/запроса.
// lastChange — когда цена (товара / минимальная в топ-N выдачи) менялась в
// последний раз; nil = новый трек без истории → ×1, даём набрать историю.
// subscribers — число активных подписчиков (кап популярности).
// Вызывающий применяет множитель ПОСЛЕ MIN по подписчикам и только вне
// reseller-дорожки (eff > resellerLaneCutoff): минутная дорожка — ядро
// ценности reseller-тарифов, бэкофф на неё не распространяется.
func VolatilityMult(lastChange *time.Time, subscribers int, now time.Time) float64 {
	if lastChange == nil {
		return 1
	}
	age := now.Sub(*lastChange)
	var m float64
	switch {
	case age < volatilityTier1:
		m = 1
	case age < volatilityTier2:
		m = 1.5
	case age < volatilityTier3:
		m = 2
	default:
		m = 3
	}
	if subscribers >= VolatilityPopularSubs && m > VolatilityPopularCap {
		m = VolatilityPopularCap
	}
	return m
}

// ApplyVolatility — eff × множитель волатильности, с потолком ceil (0 — без
// потолка). Вынесено, чтобы планировщик не жонглировал float-арифметикой.
func ApplyVolatility(eff time.Duration, lastChange *time.Time, subscribers int, now time.Time, ceil time.Duration) time.Duration {
	out := time.Duration(float64(eff) * VolatilityMult(lastChange, subscribers, now))
	if ceil > 0 && out > ceil {
		out = ceil
	}
	return out
}

// Внеплановый повтор скрейпа, когда живой цены не было (WB: сайдкар стоит без
// живых дорожек — обычно волна 498 на 30–60с). Задержка удваивается с серией
// пропусков: 2, 4, 8, 16, 30, 30… мин. При долгой стене повторы реже планового
// каданса и сайдкар не нагружают; при короткой волне товар обновится через
// пару минут, а не через 15–60 (миграция 034).
const (
	LiveRetryBase = 2 * time.Minute
	LiveRetryMax  = 30 * time.Minute
	// LiveStuckStreak — с какой серии товар считается застрявшим без цены:
	// 4 пропуска подряд = не меньше 2+4+8 = 14 мин без живой цены. Короткая
	// волна 498 (30–60с) столько не набирает.
	LiveStuckStreak = 4
)

// LiveRetryDelay — задержка повтора после streak-го пропуска подряд (streak ≥ 1).
// Зеркало формулы в ProductRepo.MarkLiveRetry.
func LiveRetryDelay(streak int) time.Duration {
	if streak < 1 {
		streak = 1
	}
	d := LiveRetryBase
	for i := 1; i < streak && d < LiveRetryMax; i++ {
		d *= 2
	}
	return min(d, LiveRetryMax)
}

// LiveRetryDue — пора ли внеплановый повтор (срок назначен и наступил).
func LiveRetryDue(retryAt *time.Time, now time.Time) bool {
	return retryAt != nil && !now.Before(*retryAt)
}
