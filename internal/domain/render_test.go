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
		// витрина продавца WB: ярлык «Магазин #{id}»
		"https://www.wildberries.ru/seller/250000206?supplier=250000206&xsubject=515": "Магазин #250000206",
		// витрина продавца + клиентский текст-фильтр tb_q
		"https://www.wildberries.ru/seller/250000206?supplier=250000206&tb_q=iphone+17": "Магазин #250000206 · iphone 17",
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
