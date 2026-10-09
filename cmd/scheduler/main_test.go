package main

import (
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
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

// Пол WB-поиска и его связь с выбором дорожки (инцидент 02-09-2026).
// Проверяем не арифметику (она тривиальна), а инвариант, ради которого выбрано
// значение 2 минуты: прижатый полом запрос ОСТАЁТСЯ на быстрой дорожке.
// Значение больше resellerLaneCutoff молча увело бы его на общую, где отброс
// устаревших выключен, — то есть перенесло бы затык, а не убрало.
func TestWBSearchFloorKeepsFastLane(t *testing.T) {
	const floor = 2 * time.Minute
	if floor > resellerLaneCutoff {
		t.Fatalf("дефолтный пол WB (%s) больше порога быстрой дорожки (%s) — "+
			"перекупские WB-запросы уедут на общую дорожку", floor, resellerLaneCutoff)
	}

	cases := []struct {
		name     string
		mp       string
		eff      time.Duration
		wantEff  time.Duration
		wantFast bool
	}{
		{"перекуп WB прижимается полом и остаётся быстрым", "wildberries", time.Minute, floor, true},
		{"обычный тариф WB полом не трогается", "wildberries", 30 * time.Minute, 30 * time.Minute, false},
		{"Ozon этот пол не касается", "ozon", time.Minute, time.Minute, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			eff := c.eff
			if c.mp == "wildberries" && eff < floor {
				eff = floor
			}
			if eff != c.wantEff {
				t.Fatalf("eff=%s, ожидалось %s", eff, c.wantEff)
			}
			if fast := eff <= resellerLaneCutoff; fast != c.wantFast {
				t.Fatalf("fast=%v, ожидалось %v (eff=%s)", fast, c.wantFast, eff)
			}
		})
	}
}

// Строки GetSchedulableProducts — по подписке: товар с двумя подписчиками
// считается один раз, не-WB не считается, застрявший — с LiveStuckStreak.
func TestReportWBLive(t *testing.T) {
	wb := "https://www.wildberries.ru/catalog/1/detail.aspx"
	reportWBLive([]postgres.SchedulableProduct{
		{ProductID: 1, URL: wb, LiveFailStreak: domain.LiveStuckStreak},
		{ProductID: 1, URL: wb, LiveFailStreak: domain.LiveStuckStreak},
		{ProductID: 2, URL: wb, LiveFailStreak: domain.LiveStuckStreak - 1},
		{ProductID: 3, URL: "https://www.ozon.ru/product/3", LiveFailStreak: 9},
	})
	if got := gaugeValue(t, metrics.WBLiveTracked); got != 2 {
		t.Fatalf("tracked = %v, ждём 2", got)
	}
	if got := gaugeValue(t, metrics.WBLiveStuck); got != 1 {
		t.Fatalf("stuck = %v, ждём 1", got)
	}
}

func gaugeValue(t *testing.T, g prometheus.Gauge) float64 {
	t.Helper()
	var m dto.Metric
	if err := g.Write(&m); err != nil {
		t.Fatal(err)
	}
	return m.GetGauge().GetValue()
}
