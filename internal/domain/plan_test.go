package domain

import (
	"testing"
	"time"
)

func TestEffectiveSearchInterval(t *testing.T) {
	const def = 20 * time.Minute

	if got := Plans["reseller"].EffectiveSearchInterval(def); got != time.Minute {
		t.Fatalf("reseller interval = %v, want 1m", got)
	}
	if got := Plans["pro"].EffectiveSearchInterval(def); got != def {
		t.Fatalf("pro interval = %v, want default %v", got, def)
	}
	if got := Plans["free"].EffectiveSearchInterval(def); got != def {
		t.Fatalf("free interval = %v, want default %v", got, def)
	}
}

func TestEffectivePlanFor(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	future := now.Add(24 * time.Hour)
	past := now.Add(-time.Hour)
	const def = 20 * time.Minute

	// Активный перекуп → быстрый интервал.
	p := EffectivePlanFor("reseller", &future, now)
	if p.Name != "reseller" || p.EffectiveSearchInterval(def) != time.Minute {
		t.Fatalf("активный reseller: got %s/%v", p.Name, p.EffectiveSearchInterval(def))
	}

	// Истёкший перекуп → free → дефолтный интервал (быстрая дорожка теряется).
	p = EffectivePlanFor("reseller", &past, now)
	if p.Name != "free" {
		t.Fatalf("истёкший reseller → ожидали free, got %s", p.Name)
	}
	if p.EffectiveSearchInterval(def) != def {
		t.Fatalf("истёкший reseller interval = %v, want default", p.EffectiveSearchInterval(def))
	}

	// Бессрочный план (nil срок).
	if p := EffectivePlanFor("reseller", nil, now); p.Name != "reseller" {
		t.Fatalf("бессрочный reseller → got %s", p.Name)
	}
}
