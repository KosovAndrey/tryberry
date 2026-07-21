package main

import (
	"testing"
	"time"
)

func TestJitterFactorRange(t *testing.T) {
	const j = 0.4
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	for id := int64(1); id <= 500; id++ {
		got := jitterFactor(id, &base, j)
		if got < 1-j || got > 1+j {
			t.Fatalf("id=%d: множитель %v вне [%v, %v]", id, got, 1-j, 1+j)
		}
	}
}

// Главное свойство: внутри одного цикла (lastEnqueued не менялся) множитель
// обязан быть одним и тем же. Иначе планировщик перерозыгрывает порог на каждом
// тике и срабатывает по первому удачному — распределение съезжает к нижней
// границе, и джиттер превращается в «всегда минимум».
func TestJitterFactorStableWithinCycle(t *testing.T) {
	last := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	want := jitterFactor(42, &last, 0.4)
	for i := 0; i < 100; i++ {
		if got := jitterFactor(42, &last, 0.4); got != want {
			t.Fatalf("тик %d: множитель поехал: %v != %v", i, got, want)
		}
	}
}

// А между циклами (lastEnqueued сдвинулся) — обязан меняться, иначе у запроса
// навсегда закрепится один интервал и сетка снова станет ровной.
func TestJitterFactorChangesBetweenCycles(t *testing.T) {
	base := time.Date(2026, 7, 21, 12, 0, 0, 0, time.UTC)
	seen := make(map[float64]int)
	for i := 0; i < 20; i++ {
		ts := base.Add(time.Duration(i) * 7 * time.Minute)
		seen[jitterFactor(7, &ts, 0.4)]++
	}
	if len(seen) < 10 {
		t.Fatalf("на 20 циклов всего %d различных множителей — плохое перемешивание", len(seen))
	}
}

func TestJitterFactorDisabled(t *testing.T) {
	ts := time.Now()
	if got := jitterFactor(1, &ts, 0); got != 1 {
		t.Fatalf("j=0 должен давать ровно 1, а дал %v", got)
	}
	// Клампим сверху: даже при абсурдном j интервал не должен схлопываться в ноль.
	if got := jitterFactor(1, &ts, 5); got < 0.1 {
		t.Fatalf("j=5 не заклампился: %v", got)
	}
}

func TestScaleDuration(t *testing.T) {
	if got := scaleDuration(10*time.Minute, 0.6); got != 6*time.Minute {
		t.Fatalf("10м × 0.6 = %v, ждали 6м", got)
	}
	if got := scaleDuration(10*time.Minute, 1.4); got != 14*time.Minute {
		t.Fatalf("10м × 1.4 = %v, ждали 14м", got)
	}
}
