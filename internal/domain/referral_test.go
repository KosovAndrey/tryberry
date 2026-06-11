package domain

import (
	"testing"
	"time"
)

func TestApplyReferralReward(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	const days = 5

	// Free → Lite на 5 дней от сейчас.
	u := &User{Plan: "free"}
	plan, exp, ok := ApplyReferralReward(u, days, now)
	if !ok || plan != "lite" || !exp.Equal(now.Add(5*24*time.Hour)) {
		t.Fatalf("free: got %s/%v/%v", plan, exp, ok)
	}

	// Триал → тоже Lite (триал не продлеваем, заменяем наградой).
	trialExp := now.Add(24 * time.Hour)
	u = &User{Plan: "trial", PlanExpiresAt: &trialExp}
	plan, _, ok = ApplyReferralReward(u, days, now)
	if !ok || plan != "lite" {
		t.Fatalf("trial: got %s/%v", plan, ok)
	}

	// Активный платный план → +5 дней к сроку, план тот же.
	proExp := now.Add(10 * 24 * time.Hour)
	u = &User{Plan: "pro", PlanExpiresAt: &proExp}
	plan, exp, ok = ApplyReferralReward(u, days, now)
	if !ok || plan != "pro" || !exp.Equal(proExp.Add(5*24*time.Hour)) {
		t.Fatalf("pro: got %s/%v/%v", plan, exp, ok)
	}

	// Истёкший платный план → действует free → Lite от сейчас.
	past := now.Add(-time.Hour)
	u = &User{Plan: "pro", PlanExpiresAt: &past}
	plan, _, ok = ApplyReferralReward(u, days, now)
	if !ok || plan != "lite" {
		t.Fatalf("expired pro: got %s/%v", plan, ok)
	}

	// Бессрочный план (выдан админом) → продлевать некуда, только аудит.
	u = &User{Plan: "unlimited"}
	if _, _, ok = ApplyReferralReward(u, days, now); ok {
		t.Fatal("unlimited: want ok=false")
	}
}
