package domain

import "testing"

func TestQueryTextFromNormalized(t *testing.T) {
	cases := map[string]string{
		// WB: параметр search=
		"https://www.wildberries.ru/catalog/0/search.aspx?search=наушники": "наушники",
		// Я.Маркет: параметр text= (+ есть hid — берём именно text)
		"https://market.yandex.ru/search?hid=90566&text=стиральная машина с сушкой": "стиральная машина с сушкой",
		// %-кодировка query-параметра декодируется
		"https://market.yandex.ru/search?text=%D0%BA%D0%BE%D1%84%D0%B5": "кофе",
		// нет ни search, ни text — отдаём строку как есть (фолбэк)
		"https://example.com/x": "https://example.com/x",
		"не-url":                "не-url",
	}
	for in, want := range cases {
		if got := QueryTextFromNormalized(in); got != want {
			t.Errorf("QueryTextFromNormalized(%q) = %q, want %q", in, got, want)
		}
	}
}
