package domain

import (
	"strings"
	"testing"
	"time"
)

func TestInSendWindow(t *testing.T) {
	// МСК = UTC+3: границы окна 10:00–21:59 МСК = 07:00–18:59 UTC.
	utc := func(h int) time.Time { return time.Date(2026, 7, 6, h, 30, 0, 0, time.UTC) }
	cases := []struct {
		utcHour int
		want    bool
	}{
		{3, false},  // 06:30 МСК — ночь
		{6, false},  // 09:30 МСК — ещё рано
		{7, true},   // 10:30 МСК — окно открылось
		{15, true},  // 18:30 МСК
		{18, true},  // 21:30 МСК — ещё можно
		{19, false}, // 22:30 МСК — поздно
		{23, false}, // 02:30 МСК
	}
	for _, c := range cases {
		if got := InSendWindow(utc(c.utcHour)); got != c.want {
			t.Errorf("utc %02d:30: got %v, want %v", c.utcHour, got, c.want)
		}
	}
}

func TestGenerateWinbackCode(t *testing.T) {
	seen := make(map[string]struct{})
	for i := 0; i < 100; i++ {
		code, err := GenerateWinbackCode()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if !strings.HasPrefix(code, "BACK30-") || len(code) != len("BACK30-")+5 {
			t.Fatalf("неожиданная форма кода: %q", code)
		}
		if code != NormalizePromoCode(code) {
			t.Fatalf("код должен быть уже в каноничной форме: %q", code)
		}
		for _, ch := range code[len("BACK30-"):] {
			if !strings.ContainsRune(winbackCodeAlphabet, ch) {
				t.Fatalf("символ %q вне алфавита в %q", ch, code)
			}
		}
		seen[code] = struct{}{}
	}
	if len(seen) < 95 { // 31^5 ≈ 28.6 млн — коллизии в сотне почти исключены
		t.Fatalf("подозрительно много коллизий: %d уникальных из 100", len(seen))
	}
}

func TestFormatMSK(t *testing.T) {
	// 12:00 UTC = 15:00 МСК.
	ts := time.Date(2026, 7, 8, 12, 0, 0, 0, time.UTC)
	if got := FormatMSK(ts); got != "08.07 в 15:00 МСК" {
		t.Errorf("got %q", got)
	}
}
