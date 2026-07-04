package main

import (
	"testing"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
)

func i64(v int64) *int64 { return &v }

func TestSynthRedirectRoute(t *testing.T) {
	s := &synthRedirect{
		tg:      []int64{111, 222},
		vk:      []int64{333},
		max:     []int64{444},
		sampleN: 10,
	}

	// Не кратный sampleN — sampled out, никаких адресатов.
	tg, vk, mx, dropped := s.route(&domain.User{ID: 7, TelegramID: 9_100_000_000_000_000_07})
	if !dropped || tg != 0 || vk != 0 || mx != 0 {
		t.Fatalf("user 7: want sampled out, got tg=%d vk=%d max=%d dropped=%v", tg, vk, mx, dropped)
	}

	// Кратный sampleN, TG-only синтетик → один из тест-TG-чатов, детерминированно.
	u := &domain.User{ID: 20, TelegramID: 9_100_000_000_000_000_20}
	tg1, vk1, mx1, dropped := s.route(u)
	if dropped || tg1 == 0 || vk1 != 0 || mx1 != 0 {
		t.Fatalf("user 20 tg-only: got tg=%d vk=%d max=%d dropped=%v", tg1, vk1, mx1, dropped)
	}
	tg2, _, _, _ := s.route(u)
	if tg1 != tg2 {
		t.Fatalf("routing must be deterministic: %d != %d", tg1, tg2)
	}

	// VK+MAX синтетик без TG → только vk/max редирект.
	tg, vk, mx, dropped = s.route(&domain.User{ID: 30, VKID: i64(1), MaxID: i64(2)})
	if dropped || tg != 0 || vk != 333 || mx != 444 {
		t.Fatalf("user 30 vk+max: got tg=%d vk=%d max=%d dropped=%v", tg, vk, mx, dropped)
	}

	// sampleN=1 — форвардим всех.
	all := &synthRedirect{tg: []int64{111}, sampleN: 1}
	if _, _, _, dropped := all.route(&domain.User{ID: 7, TelegramID: 1}); dropped {
		t.Fatal("sampleN=1 must forward every user")
	}
}

func TestParseSynthRedirect(t *testing.T) {
	// Без env — выключен.
	if s := parseSynthRedirect(); s != nil {
		t.Fatalf("no env: want nil, got %+v", s)
	}

	t.Setenv("SYNTH_REDIRECT_TG_IDS", "123, 456,")
	t.Setenv("SYNTH_REDIRECT_MAX_IDS", "789")
	t.Setenv("SYNTH_REDIRECT_SAMPLE_N", "5")
	s := parseSynthRedirect()
	if s == nil {
		t.Fatal("want config, got nil")
	}
	if len(s.tg) != 2 || s.tg[0] != 123 || s.tg[1] != 456 {
		t.Fatalf("tg: %v", s.tg)
	}
	if len(s.vk) != 0 || len(s.max) != 1 || s.max[0] != 789 {
		t.Fatalf("vk/max: %v %v", s.vk, s.max)
	}
	if s.sampleN != 5 {
		t.Fatalf("sampleN: %d", s.sampleN)
	}
}
