package domain

import (
	"testing"
	"time"
)

func TestAssessHonestPrice(t *testing.T) {
	now := time.Now()
	old := now.Add(-30 * 24 * time.Hour) // наблюдаем дольше honestMinAge
	// Базовый набор: достаточно возраста; min30=100, median30=150, min90=90, minAll=80.
	full := PriceStats{
		Min30: 100, Median30: 150, Min90: 90, MinAll: 80,
		Seg30: 4, CountAll: 50, Since: old, HasData: true,
	}

	cases := []struct {
		name    string
		current float64
		stats   PriceStats
		want    PriceVerdict
	}{
		{"минимум за всё время", 80, full, VerdictLowestEver},
		{"ниже минимума всего → тоже lowest ever", 75, full, VerdictLowestEver},
		{"минимум за 90д", 90, full, VerdictLowest90},
		{"минимум за 30д", 100, full, VerdictLowest30},
		{"в районе медианы", 150, full, VerdictTypical},
		{"чуть ниже медианы", 140, full, VerdictTypical},
		{"выше медианы — завышенная скидка", 170, full, VerdictAboveTypical},
		{"стабильная цена, мало записей, но возраст ок", 100, PriceStats{
			Min30: 100, Median30: 100, Min90: 100, MinAll: 100,
			Seg30: 1, CountAll: 1, Since: old, HasData: true,
		}, VerdictLowestEver},
		{"молодая история (< honestMinAge)", 100, PriceStats{
			Min30: 100, Median30: 150, MinAll: 80, Seg30: 5, CountAll: 5,
			Since: now.Add(-2 * 24 * time.Hour), HasData: true,
		}, VerdictInsufficient},
		// Гейт поднят с 7 до 14 дней: десятидневное наблюдение больше не даёт
		// вердикта. Раньше этот же набор классифицировался как AboveTypical.
		{"10 дней наблюдения — уже не хватает (гейт 14д)", 170, PriceStats{
			Min30: 100, Median30: 150, Min90: 90, MinAll: 80, Seg30: 4, CountAll: 20,
			Since: now.Add(-10 * 24 * time.Hour), HasData: true,
		}, VerdictInsufficient},
		{"15 дней наблюдения — уже хватает", 170, PriceStats{
			Min30: 100, Median30: 150, Min90: 90, MinAll: 80, Seg30: 4, CountAll: 20,
			Since: now.Add(-15 * 24 * time.Hour), HasData: true,
		}, VerdictAboveTypical},
		{"нет данных", 100, PriceStats{}, VerdictInsufficient},
		{"нулевая цена", 0, full, VerdictInsufficient},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := AssessHonestPrice(c.current, c.stats, now).Verdict
			if got != c.want {
				t.Errorf("AssessHonestPrice(%.0f) verdict=%d; want %d", c.current, got, c.want)
			}
		})
	}
}

// Line: зелёные/жёлтый вердикты дают непустую строку, Insufficient — пустую,
// AboveTypical несёт медиану (опорное число для пользователя).
func TestHonestPriceLine(t *testing.T) {
	if got := (HonestPrice{Verdict: VerdictInsufficient}).Line(); got != "" {
		t.Errorf("Insufficient.Line()=%q; хотим пусто", got)
	}
	for _, v := range []PriceVerdict{VerdictLowestEver, VerdictLowest90, VerdictLowest30, VerdictTypical} {
		if (HonestPrice{Verdict: v}).Line() == "" {
			t.Errorf("verdict %d дал пустую строку", v)
		}
	}
	line := (HonestPrice{Verdict: VerdictAboveTypical, Median30: 150}).Line()
	if line == "" || !contains(line, "150") {
		t.Errorf("AboveTypical.Line()=%q; хотим с медианой 150", line)
	}
}

// Окно медианы в тексте называется честно: пока наблюдение короче 30 дней,
// подставляем фактический срок, а не номинальные «30 дней».
func TestHonestPriceLineMedianWindow(t *testing.T) {
	day := 24 * time.Hour

	cases := []struct {
		name     string
		observed time.Duration
		want     string
	}{
		{"наблюдение длиннее окна — номинальные 30 дней", 45 * day, "30 дней"},
		{"ровно 30 дней — номинальные", 30 * day, "30 дней"},
		{"неизвестный срок — не выдумываем", 0, "30 дней"},
		{"17 дней", 17 * day, "17 дней наблюдения"},
		{"21 день — склонение", 21 * day, "21 день наблюдения"},
		{"22 дня — склонение", 22 * day, "22 дня наблюдения"},
		{"14 дней — склонение", 14 * day, "14 дней наблюдения"},
		{"меньше суток — минимум один день", 5 * time.Hour, "1 день наблюдения"},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			hp := HonestPrice{Verdict: VerdictAboveTypical, Median30: 150, Observed: c.observed}
			got := hp.Line()
			if !contains(got, c.want) {
				t.Errorf("Line()=%q; хотим подстроку %q", got, c.want)
			}
		})
	}
}

// Observed заполняется из Since — иначе рендер не сможет назвать честное окно.
func TestAssessHonestPriceFillsObserved(t *testing.T) {
	now := time.Now()
	s := PriceStats{
		Min30: 100, Median30: 150, Min90: 90, MinAll: 80,
		Seg30: 4, CountAll: 20, Since: now.Add(-17 * 24 * time.Hour), HasData: true,
	}
	if got := AssessHonestPrice(170, s, now).Observed; got < 16*24*time.Hour || got > 18*24*time.Hour {
		t.Errorf("Observed=%v; хотим ≈17 дней", got)
	}
	if got := AssessHonestPrice(170, PriceStats{}, now).Observed; got != 0 {
		t.Errorf("Observed без Since = %v; хотим 0", got)
	}
}

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
