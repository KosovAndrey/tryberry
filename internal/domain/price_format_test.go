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

// Разделитель — именно неразрывный пробел: в мессенджерах обычный рвёт число
// переносом строки («75\n000 ₽»), ради чего хелпер и заводился.
func TestFormatPriceUsesNonBreakingSpace(t *testing.T) {
	got := FormatPrice(75000)
	if got != "75 000 ₽" {
		t.Errorf("FormatPrice(75000) = %q; want %q", got, "75 000 ₽")
	}
	for _, r := range got {
		if r == ' ' {
			t.Errorf("в %q попал обычный пробел", got)
		}
	}
}
