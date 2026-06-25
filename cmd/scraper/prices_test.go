package main

import "testing"

func TestPricesEqual_KopeckNoise(t *testing.T) {
	// Корневой кейс бага: одна и та же копеечная цена, посчитанная делением
	// (как в WB-скрейпере) и умножением на 10^-2 (как pgx из NUMERIC), даёт разные
	// float64 — строгое != ломалось, pricesEqual должен считать их равными.
	div := float64(220129) / 100   // WB: kopecks/100
	mult := float64(220129) * 1e-2 // pgx: int * 10^exp
	if div == mult {
		t.Skip("на этой платформе деление и умножение совпали — тест неинформативен")
	}
	if !pricesEqual(div, mult) {
		t.Errorf("pricesEqual(%.17g, %.17g) = false, ожидали true (одна цена 2201.29)", div, mult)
	}
}

func TestPricesEqual(t *testing.T) {
	cases := []struct {
		a, b float64
		want bool
	}{
		{2201.29, 2201.29, true},
		{2201.29, 2201.30, false}, // реальное изменение на копейку
		{100, 100, true},
		{100, 100.004, true},  // меньше полукопейки — шум
		{100, 100.006, false}, // ≥ полукопейки — округлится до 100.01
		{0, 0, true},
	}
	for _, c := range cases {
		if got := pricesEqual(c.a, c.b); got != c.want {
			t.Errorf("pricesEqual(%v, %v) = %v, want %v", c.a, c.b, got, c.want)
		}
	}
}
