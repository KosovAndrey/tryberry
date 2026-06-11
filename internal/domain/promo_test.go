package domain

import (
	"errors"
	"testing"
	"time"
)

func TestApplyGrantPromo(t *testing.T) {
	now := time.Date(2026, 6, 11, 12, 0, 0, 0, time.UTC)
	promo := PromoCode{Kind: PromoKindGrant, Plan: "pro", Days: 7}

	// Free → план кода на 7 дней от сейчас.
	u := &User{Plan: "free"}
	exp, err := ApplyGrantPromo(u, promo, now)
	if err != nil {
		t.Fatalf("free: unexpected err %v", err)
	}
	if want := now.Add(7 * 24 * time.Hour); !exp.Equal(want) {
		t.Fatalf("free: exp = %v, want %v", exp, want)
	}

	// Триал → промо замещает (триал считается бесплатным состоянием).
	trialExp := now.Add(24 * time.Hour)
	u = &User{Plan: "trial", PlanExpiresAt: &trialExp}
	exp, err = ApplyGrantPromo(u, promo, now)
	if err != nil {
		t.Fatalf("trial: unexpected err %v", err)
	}
	if want := now.Add(7 * 24 * time.Hour); !exp.Equal(want) {
		t.Fatalf("trial: exp = %v, want %v", exp, want)
	}

	// Истёкший платный план → действует free → промо применяется.
	past := now.Add(-time.Hour)
	u = &User{Plan: "lite", PlanExpiresAt: &past}
	if _, err := ApplyGrantPromo(u, promo, now); err != nil {
		t.Fatalf("expired lite: unexpected err %v", err)
	}

	// Тот же план активен → продление от текущего срока.
	proExp := now.Add(48 * time.Hour)
	u = &User{Plan: "pro", PlanExpiresAt: &proExp}
	exp, err = ApplyGrantPromo(u, promo, now)
	if err != nil {
		t.Fatalf("extend pro: unexpected err %v", err)
	}
	if want := proExp.Add(7 * 24 * time.Hour); !exp.Equal(want) {
		t.Fatalf("extend pro: exp = %v, want %v", exp, want)
	}

	// Другой платный план активен → конфликт.
	liteExp := now.Add(48 * time.Hour)
	u = &User{Plan: "lite", PlanExpiresAt: &liteExp}
	if _, err := ApplyGrantPromo(u, promo, now); !errors.Is(err, ErrPromoPlanConflict) {
		t.Fatalf("lite active: err = %v, want ErrPromoPlanConflict", err)
	}

	// Тот же план без срока (бессрочный от админа) → конфликт, продлевать некуда.
	u = &User{Plan: "pro"}
	if _, err := ApplyGrantPromo(u, promo, now); !errors.Is(err, ErrPromoPlanConflict) {
		t.Fatalf("infinite pro: err = %v, want ErrPromoPlanConflict", err)
	}
}

func TestNormalizePromoCode(t *testing.T) {
	if got := NormalizePromoCode("  launch7 "); got != "LAUNCH7" {
		t.Fatalf("got %q, want LAUNCH7", got)
	}
}
