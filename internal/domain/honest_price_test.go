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

func contains(s, sub string) bool {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}
