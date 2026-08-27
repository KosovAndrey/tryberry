package domain

import (
	"testing"
	"time"
)

func TestHumanizeAge(t *testing.T) {
	cases := []struct {
		d    time.Duration
		want string
	}{
		{0, "только что"},
		{-5 * time.Minute, "только что"}, // часы сервера разъехались
		{90 * time.Second, "только что"},
		{2 * time.Minute, "2 минуты назад"},
		{time.Minute * 21, "21 минуту назад"},
		{time.Minute * 11, "11 минут назад"},
		{time.Minute * 45, "45 минут назад"},
		{time.Hour, "1 час назад"},
		{3 * time.Hour, "3 часа назад"},
		{5 * time.Hour, "5 часов назад"},
		{23 * time.Hour, "23 часа назад"},
		{24 * time.Hour, "1 день назад"},
		{50 * time.Hour, "2 дня назад"},
		{11 * 24 * time.Hour, "11 дней назад"},
		{21 * 24 * time.Hour, "21 день назад"},
	}
	for _, c := range cases {
		if got := HumanizeAge(c.d); got != c.want {
			t.Errorf("HumanizeAge(%v) = %q; want %q", c.d, got, c.want)
		}
	}
}
