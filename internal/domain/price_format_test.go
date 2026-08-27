package domain

import "testing"

func TestFormatPrice(t *testing.T) {
	cases := []struct {
		v    float64
		want string
	}{
		{0, "0 ₽"},
		{999, "999 ₽"},
		{1000, "1 000 ₽"},
		{75000, "75 000 ₽"},
		{1234567, "1 234 567 ₽"},
		{1499.6, "1 500 ₽"}, // округление как у %.0f
		{-2500, "-2 500 ₽"}, // скидка «-2 500 ₽»
	}
	for _, c := range cases {
		if got := FormatPrice(c.v); got != c.want {
			t.Errorf("FormatPrice(%v) = %q; want %q", c.v, got, c.want)
		}
	}
}
