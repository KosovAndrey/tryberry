package domain

import (
	"testing"
	"time"
)

func TestShowTrialOffer(t *testing.T) {
	// free (1 поиск @6ч) и безпоисковый legacy basic — предлагаем триал;
	// платные с поиском — нет. Регрессия: после free MaxSearch 0→1 условие
	// `MaxSearch==0` перестало ловить free (см. ShowTrialOffer).
	cases := map[string]bool{
		"free":           true,
		"basic":          true, // legacy, MaxSearch=0
		"lite":           false,
		"pro":            false,
		"trial":          false,
		"reseller_start": false,
		"reseller_pro":   false,
	}
	for name, want := range cases {
		if got := Plans[name].ShowTrialOffer(); got != want {
			t.Errorf("ShowTrialOffer(%s) = %v, want %v", name, got, want)
		}
	}
}

func TestBundleWindow(t *testing.T) {
	cases := map[string]time.Duration{
		"free":           15 * time.Minute,
		"basic":          15 * time.Minute,
		"lite":           10 * time.Minute,
		"pro":            5 * time.Minute,
		"trial":          5 * time.Minute,
		"reseller_start": 0,
		"reseller_pro":   0,
		"reseller":       0,
		"unlimited":      0,
	}
	for name, want := range cases {
		if got := Plans[name].BundleWindow(); got != want {
			t.Errorf("BundleWindow(%s) = %v, want %v", name, got, want)
		}
	}
	// Неизвестный план → дефолт 15м.
	if got := (Plan{Name: "???"}).BundleWindow(); got != 15*time.Minute {
		t.Errorf("unknown plan BundleWindow = %v, want 15m", got)
	}
}

func TestEffectiveInterval(t *testing.T) {
	const def = 15 * time.Minute

	cases := map[string]time.Duration{
		"free":           60 * time.Minute,
		"trial":          15 * time.Minute,
		"lite":           30 * time.Minute,
		"pro":            15 * time.Minute,
		"reseller_start": time.Minute,
		"reseller_pro":   time.Minute,
	}
	for name, want := range cases {
		if got := Plans[name].EffectiveInterval(def); got != want {
			t.Fatalf("%s interval = %v, want %v", name, got, want)
		}
	}

	// План без своего интервала (Interval==0) → дефолт-фолбэк.
	if got := (Plan{}).EffectiveInterval(def); got != def {
		t.Fatalf("zero-interval plan = %v, want default %v", got, def)
	}
}

func TestEffectivePlanFor(t *testing.T) {
	now := time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-time.Hour)
	const def = 15 * time.Minute

	// Активный reseller_pro → быстрый интервал.
	p := EffectivePlanFor("reseller_pro", &future, now)
	if p.Name != "reseller_pro" || p.EffectiveInterval(def) != time.Minute {
		t.Fatalf("активный reseller_pro: got %s/%v", p.Name, p.EffectiveInterval(def))
	}

	// Истёкший reseller_pro → free → 60 мин (быстрая дорожка теряется).
	p = EffectivePlanFor("reseller_pro", &past, now)
	if p.Name != "free" {
		t.Fatalf("истёкший reseller_pro → ожидали free, got %s", p.Name)
	}
	if p.EffectiveInterval(def) != 60*time.Minute {
		t.Fatalf("истёкший reseller_pro interval = %v, want 60m", p.EffectiveInterval(def))
	}

	// Бессрочный план (nil срок).
	if p := EffectivePlanFor("pro", nil, now); p.Name != "pro" {
		t.Fatalf("бессрочный pro → got %s", p.Name)
	}

	// Legacy-алиасы ещё распознаются (до миграции имён в БД).
	if p := EffectivePlanFor("reseller", &future, now); p.Name != "reseller" {
		t.Fatalf("legacy reseller → got %s", p.Name)
	}
	if p := EffectivePlanFor("basic", &future, now); p.Name != "basic" {
		t.Fatalf("legacy basic → got %s", p.Name)
	}
}
