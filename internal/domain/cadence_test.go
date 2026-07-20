package domain

import (
	"testing"
	"time"
)

func TestVolatilityMult(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time {
		ts := now.Add(-d)
		return &ts
	}

	cases := []struct {
		name       string
		lastChange *time.Time
		subs       int
		want       float64
	}{
		{"новый трек (nil)", nil, 1, 1},
		{"менялась только что", ago(time.Hour), 1, 1},
		{"на границе 5 дней ещё ×1", ago(5*24*time.Hour - time.Second), 1, 1},
		{"5 дней — ×1.5", ago(5 * 24 * time.Hour), 1, 1.5},
		{"9 дней — ×1.5", ago(9 * 24 * time.Hour), 1, 1.5},
		{"10 дней — ×2", ago(10 * 24 * time.Hour), 1, 2},
		{"19 дней — ×2", ago(19 * 24 * time.Hour), 1, 2},
		{"20 дней — ×3", ago(20 * 24 * time.Hour), 1, 3},
		{"полгода — всё равно ×3 (потолок)", ago(180 * 24 * time.Hour), 1, 3},
		{"популярный (5 подписчиков) капится ×2", ago(60 * 24 * time.Hour), 5, 2},
		{"популярный не мешает ×1.5", ago(6 * 24 * time.Hour), 9, 1.5},
		{"4 подписчика — ещё не популярный", ago(60 * 24 * time.Hour), 4, 3},
	}
	for _, c := range cases {
		if got := VolatilityMult(c.lastChange, c.subs, now); got != c.want {
			t.Errorf("%s: got ×%v, want ×%v", c.name, got, c.want)
		}
	}
}

func TestApplyVolatility(t *testing.T) {
	now := time.Date(2026, 7, 6, 12, 0, 0, 0, time.UTC)
	stale := now.Add(-30 * 24 * time.Hour) // ×3

	// Товарный путь: без потолка.
	if got := ApplyVolatility(15*time.Minute, &stale, 1, now, 0); got != 45*time.Minute {
		t.Errorf("pro товар: got %v, want 45m", got)
	}
	// Поиск: потолок 12ч режет 6ч×3=18ч.
	if got := ApplyVolatility(6*time.Hour, &stale, 1, now, SearchIntervalCeil); got != 12*time.Hour {
		t.Errorf("free поиск: got %v, want 12h", got)
	}
	// Дробный множитель ×1.5.
	fresh := now.Add(-6 * 24 * time.Hour)
	if got := ApplyVolatility(30*time.Minute, &fresh, 1, now, 0); got != 45*time.Minute {
		t.Errorf("lite товар ×1.5: got %v, want 45m", got)
	}
	// Новый трек — интервал не трогаем.
	if got := ApplyVolatility(time.Hour, nil, 1, now, SearchIntervalCeil); got != time.Hour {
		t.Errorf("новый трек: got %v, want 1h", got)
	}
}

func TestEffectiveSearchInterval(t *testing.T) {
	def := 15 * time.Minute
	// free задаёт отдельный поиск-интервал.
	if got := Plans["free"].EffectiveSearchInterval(def); got != 6*time.Hour {
		t.Errorf("free: got %v, want 6h", got)
	}
	// Платные планы без SearchInterval — фолбэк в обычный Interval.
	if got := Plans["pro"].EffectiveSearchInterval(def); got != 15*time.Minute {
		t.Errorf("pro: got %v, want 15m", got)
	}
	if got := Plans["reseller_pro"].EffectiveSearchInterval(def); got != time.Minute {
		t.Errorf("reseller_pro: got %v, want 1m", got)
	}
	// Пустой план — дефолт.
	if got := (Plan{}).EffectiveSearchInterval(def); got != def {
		t.Errorf("empty plan: got %v, want %v", got, def)
	}
}

func TestEffectiveSearchCooldown(t *testing.T) {
	def := 6 * time.Hour
	// free: зазор равен его же кадансу — поведение до фикса.
	if got := Plans["free"].EffectiveSearchCooldown(def); got != 6*time.Hour {
		t.Errorf("free: got %v, want 6h", got)
	}
	// Платные — короче, чем дефолтные 6ч.
	if got := Plans["pro"].EffectiveSearchCooldown(def); got != time.Hour {
		t.Errorf("pro: got %v, want 1h", got)
	}
	// Перекупы — зазора нет: частоту ограничивает только каданс скрейпа.
	// Регрессия основного бага: единый зазор 6ч съедал минутный тариф.
	if got := Plans["reseller_pro"].EffectiveSearchCooldown(def); got != 0 {
		t.Errorf("reseller_pro: got %v, want 0 (off)", got)
	}
	// План без своего значения (legacy basic) — фолбэк на env-ручку.
	if got := Plans["basic"].EffectiveSearchCooldown(def); got != def {
		t.Errorf("basic: got %v, want %v", got, def)
	}
	if got := (Plan{}).EffectiveSearchCooldown(def); got != def {
		t.Errorf("empty plan: got %v, want %v", got, def)
	}
}
