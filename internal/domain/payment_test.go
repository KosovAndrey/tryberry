package domain

import (
	"testing"
	"time"
)

func TestApplyPurchase(t *testing.T) {
	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	day := 24 * time.Hour

	t.Run("free → now+days", func(t *testing.T) {
		u := &User{Plan: "free"}
		got := ApplyPurchase(u, "pro", 30, now)
		if want := now.Add(30 * day); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("тот же активный план продлевается от срока", func(t *testing.T) {
		exp := now.Add(5 * day)
		u := &User{Plan: "pro", PlanExpiresAt: &exp}
		got := ApplyPurchase(u, "pro", 30, now)
		if want := exp.Add(30 * day); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("истёкший тот же план → now+days", func(t *testing.T) {
		exp := now.Add(-day)
		u := &User{Plan: "pro", PlanExpiresAt: &exp}
		got := ApplyPurchase(u, "pro", 30, now)
		if want := now.Add(30 * day); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})

	t.Run("другой активный план → now+days (без конвертаций)", func(t *testing.T) {
		exp := now.Add(10 * day)
		u := &User{Plan: "lite", PlanExpiresAt: &exp}
		got := ApplyPurchase(u, "pro", 30, now)
		if want := now.Add(30 * day); !got.Equal(want) {
			t.Fatalf("got %v, want %v", got, want)
		}
	})
}

func TestDiscountedKopecks(t *testing.T) {
	cases := []struct {
		kop  int64
		pct  int
		want int64
	}{
		{19900, 0, 19900},
		{19900, 20, 15900}, // 159.20 → 159.00: вниз до рубля (СБП не ест копейки)
		{19900, 100, 0},
		{19900, -5, 19900},
		{49900, 15, 42400}, // 424.15 → 424.00
		{19900, 50, 9900},  // 99.50 → 99.00 (кейс 50%-промокода)
		{18900, 50, 9400},  // 94.50 → 94.00 (sub-цена lite)
	}
	for _, c := range cases {
		if got := DiscountedKopecks(c.kop, c.pct); got != c.want {
			t.Errorf("DiscountedKopecks(%d, %d) = %d, want %d", c.kop, c.pct, got, c.want)
		}
	}
}

func TestKopecksToRubString(t *testing.T) {
	cases := []struct {
		kop  int64
		want string
	}{
		{19900, "199.00"},
		{15920, "159.20"},
		{99, "0.99"},
		{0, "0.00"},
		{199000, "1990.00"},
	}
	for _, c := range cases {
		if got := KopecksToRubString(c.kop); got != c.want {
			t.Errorf("KopecksToRubString(%d) = %q, want %q", c.kop, got, c.want)
		}
	}
}
