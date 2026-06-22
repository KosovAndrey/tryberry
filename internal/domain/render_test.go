package domain

import (
	"strings"
	"testing"
)

func TestHasTextFilter(t *testing.T) {
	cases := map[string]bool{
		"https://www.wildberries.ru/seller/100":                           false,
		"https://www.wildberries.ru/seller/100?xsubject=515":              false,
		"https://www.wildberries.ru/seller/100?tb_q=iphone":               true,
		"https://www.wildberries.ru/seller/100?tb_q=%20&x=1":              false, // пробельный tb_q не считается
		"https://www.wildberries.ru/seller/100?xsubject=5&tb_q=iphone+17": true,
	}
	for in, want := range cases {
		if got := HasTextFilter(in); got != want {
			t.Errorf("HasTextFilter(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestAppendTextFilter(t *testing.T) {
	// Без query — добавляется первым параметром.
	if got := AppendTextFilter("https://www.wildberries.ru/seller/100", "iphone 17"); !strings.Contains(got, "tb_q=iphone+17") {
		t.Errorf("AppendTextFilter без query: %q", got)
	}

	// Ключевой кейс: фильтр WB с «;» НЕ должен потеряться (RawQuery дополняем
	// напрямую, а не через url.Query/Encode, который отбрасывает пары с «;»).
	got := AppendTextFilter(
		"https://www.wildberries.ru/seller/100?xsubject=515&f5023=1345910513;2546296969",
		"iphone 17")
	if !strings.Contains(got, "f5023=1345910513;2546296969") {
		t.Errorf("фильтр с «;» потерян: %q", got)
	}
	if !strings.Contains(got, "xsubject=515") {
		t.Errorf("фильтр xsubject потерян: %q", got)
	}
	if !strings.Contains(got, "tb_q=iphone+17") {
		t.Errorf("tb_q не добавлен: %q", got)
	}
	// И HasTextFilter теперь его видит.
	if !HasTextFilter(got) {
		t.Errorf("HasTextFilter после AppendTextFilter = false: %q", got)
	}
}

func TestIsSellerVanityURL(t *testing.T) {
	cases := map[string]bool{
		"https://www.wildberries.ru/seller/moderndevice":            true, // буквенный слаг
		"https://www.wildberries.ru/seller/moderndevice?x=1":        true,
		"https://www.wildberries.ru/seller/250021611":               false, // числовой — обычный флоу
		"https://www.wildberries.ru/catalog/0/search.aspx?search=x": false,
		"просто текст":                                              false,
	}
	for in, want := range cases {
		if got := IsSellerVanityURL(in); got != want {
			t.Errorf("IsSellerVanityURL(%q) = %v, want %v", in, got, want)
		}
	}
}

func TestSellerLabel(t *testing.T) {
	base := "https://www.wildberries.ru/seller/100"
	if got := SellerLabel("Купибара", base); got != "Купибара" {
		t.Errorf("SellerLabel без фильтра = %q, want «Купибара»", got)
	}
	if got := SellerLabel("Купибара", base+"?tb_q=iphone+17"); got != "Купибара · iphone 17" {
		t.Errorf("SellerLabel с фильтром = %q", got)
	}
	if got := SellerLabel("", base); got != "Магазин" {
		t.Errorf("SellerLabel без имени = %q, want «Магазин»", got)
	}
}

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
