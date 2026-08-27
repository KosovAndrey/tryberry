package domain

import (
	"testing"
	"time"
)

func TestStaleSearchNote(t *testing.T) {
	now := time.Date(2026, 8, 28, 12, 0, 0, 0, time.UTC)
	created := now.Add(-30 * 24 * time.Hour)
	at := func(d time.Duration) *time.Time { t := now.Add(-d); return &t }

	cases := []struct {
		name     string
		last     *time.Time
		created  time.Time
		interval time.Duration
		wantNote bool
	}{
		{"pro свежая выдача", at(20 * time.Minute), created, 15 * time.Minute, false},
		{"pro два пропуска — ещё норма", at(40 * time.Minute), created, 15 * time.Minute, false},
		{"pro четыре часа тишины", at(4 * time.Hour), created, 15 * time.Minute, true},
		// У free интервал 6ч, три цикла = 18ч: четыре часа для него не повод ругаться.
		{"free четыре часа — норма", at(4 * time.Hour), created, 6 * time.Hour, false},
		{"free сутки тишины", at(24 * time.Hour), created, 6 * time.Hour, true},
		// Пол у порога: у reseller интервал минута, но раньше часа не ругаемся.
		{"reseller десять минут — норма", at(10 * time.Minute), created, time.Minute, false},
		{"reseller два часа", at(2 * time.Hour), created, time.Minute, true},
		// Ни разу не скрейпили: отсчёт от создания подписки.
		{"новая подписка без выдачи", nil, now.Add(-5 * time.Minute), 15 * time.Minute, false},
		{"старая подписка без выдачи", nil, now.Add(-3 * time.Hour), 15 * time.Minute, true},
	}
	for _, c := range cases {
		got := StaleSearchNote(c.last, c.created, c.interval, now)
		if (got != "") != c.wantNote {
			t.Errorf("%s: StaleSearchNote = %q; ждали пометку=%v", c.name, got, c.wantNote)
		}
	}
}
