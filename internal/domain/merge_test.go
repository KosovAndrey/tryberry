package domain

import (
	"testing"
	"time"
)

func userWith(plan string, exp *time.Time) *User {
	return &User{Plan: plan, PlanExpiresAt: exp}
}

func TestComputeMerge(t *testing.T) {
	now := time.Date(2026, 6, 12, 12, 0, 0, 0, time.UTC)
	days := func(n float64) *time.Time {
		t := now.Add(time.Duration(n * 24 * float64(time.Hour)))
		return &t
	}

	t.Run("free+free", func(t *testing.T) {
		d := ComputeMerge(userWith("free", nil), userWith("free", nil), now)
		if d.NeedChoice || d.Options[0].Plan != "free" {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("trial+trial берёт больший остаток", func(t *testing.T) {
		d := ComputeMerge(userWith("trial", days(2)), userWith("trial", days(6)), now)
		if d.NeedChoice || d.Options[0].Plan != "trial" || d.Options[0].Days != 6 {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("trial+paid → paid как есть", func(t *testing.T) {
		d := ComputeMerge(userWith("trial", days(6)), userWith("lite", days(10)), now)
		if d.NeedChoice || d.Options[0].Plan != "lite" || d.Options[0].Days != 10 {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("истёкший paid = free → trial выживает", func(t *testing.T) {
		past := now.Add(-24 * time.Hour)
		d := ComputeMerge(userWith("pro", &past), userWith("trial", days(3)), now)
		if d.NeedChoice || d.Options[0].Plan != "trial" || d.Options[0].Days != 3 {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("одинаковые платные складываются", func(t *testing.T) {
		d := ComputeMerge(userWith("pro", days(10)), userWith("pro", days(5)), now)
		if d.NeedChoice || d.Options[0].Plan != "pro" || d.Options[0].Days != 15 {
			t.Fatalf("got %+v", d)
		}
	})

	t.Run("lite25+pro25 → выбор 35/88 (пример из ТЗ)", func(t *testing.T) {
		d := ComputeMerge(userWith("lite", days(25)), userWith("pro", days(25)), now)
		if !d.NeedChoice || len(d.Options) != 2 {
			t.Fatalf("got %+v", d)
		}
		if d.Options[0].Plan != "pro" || d.Options[0].Days != 35 {
			t.Fatalf("дорогой вариант: got %+v", d.Options[0])
		}
		// 25 + 25*(499/199) = 87.69 → 88
		if d.Options[1].Plan != "lite" || d.Options[1].Days != 88 {
			t.Fatalf("дешёвый вариант: got %+v", d.Options[1])
		}
	})

	t.Run("unlimited бессрочный побеждает без выбора", func(t *testing.T) {
		d := ComputeMerge(userWith("unlimited", nil), userWith("pro", days(25)), now)
		if d.NeedChoice || d.Options[0].Plan != "unlimited" || d.Options[0].ExpiresAt != nil {
			t.Fatalf("got %+v", d)
		}
	})
}
