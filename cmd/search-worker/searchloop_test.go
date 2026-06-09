package main

import (
	"testing"
	"time"
)

func TestShouldEvaluate(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	const (
		reseller = time.Minute
		normal   = 20 * time.Minute
	)

	cases := []struct {
		name     string
		lastEval *time.Time
		interval time.Duration
		want     bool
	}{
		{"никогда не оценивалась", nil, normal, true},
		{"перекуп: прошла минута → оцениваем", ago(60 * time.Second), reseller, true},
		{"перекуп: чуть раньше (в пределах slack) → оцениваем", ago(58 * time.Second), reseller, true},
		{"перекуп: 10с назад → ждём", ago(10 * time.Second), reseller, false},
		// Обычный подписчик на перекуп-запросе: выдача скрейпится раз в минуту,
		// но оценка не чаще ~20 мин — фаст-частота к нему не протекает.
		{"обычный: 1 минута назад → ждём (обузили)", ago(time.Minute), normal, false},
		{"обычный: 5 минут назад → ждём", ago(5 * time.Minute), normal, false},
		{"обычный: 20 минут назад → оцениваем", ago(20 * time.Minute), normal, true},
		{"обычный: 19м58с (в пределах slack) → оцениваем", ago(19*time.Minute + 58*time.Second), normal, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldEvaluate(c.lastEval, c.interval, now); got != c.want {
				t.Fatalf("shouldEvaluate=%v, want %v", got, c.want)
			}
		})
	}
}
