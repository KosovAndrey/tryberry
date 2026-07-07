package scraper

import (
	"encoding/json"
	"testing"
)

// TestWBBasketNumberMeasured — таблица «vol → реальный basket», измеренная
// пробой живых артикулов 2026-07-07 (см. experiments/wb-basket-probe/results.txt).
// Сторожит от регрессий калибровки шардов; nmID = vol*100000 (номер шарда зависит
// только от vol).
func TestWBBasketNumberMeasured(t *testing.T) {
	cases := []struct {
		vol    int64
		basket int64
	}{
		{6410, 31}, {6700, 32}, {6875, 33}, {6972, 33}, {7068, 34},
		{7280, 34}, {7584, 35}, {7825, 36}, {7949, 36}, {8015, 37},
		{8074, 37}, {8326, 38}, {8741, 38}, {8844, 39}, {8943, 39},
		{9007, 39}, {9429, 40}, {9780, 41}, {10284, 41}, {10961, 42},
		{11117, 42}, {11420, 43}, {11625, 43},
	}
	for _, c := range cases {
		id := c.vol * 100000
		if got := wbBasketNumber(id); got != c.basket {
			t.Errorf("wbBasketNumber(vol %d) = %d; измерено %d", c.vol, got, c.basket)
		}
	}
}

func TestBasketCandidates(t *testing.T) {
	// Кандидат первым, затем ближайшие соседи (±1, ±2 …) — для vol8943 формула даёт
	// 40, а реальный шард 39, он должен пробоваться сразу после кандидата.
	got := basketCandidates(40, 4)
	want := []int64{40, 39, 41, 38, 42, 37, 43, 36, 44}
	if len(got) != len(want) {
		t.Fatalf("basketCandidates(40,4) len=%d %v; want %v", len(got), got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("basketCandidates(40,4)=%v; want %v", got, want)
		}
	}
	// Номера <1 отбрасываются (без 0 и отрицательных).
	for _, b := range basketCandidates(2, 4) {
		if b < 1 {
			t.Errorf("basketCandidates(2,4) дал номер <1: %v", basketCandidates(2, 4))
		}
	}
}

func TestUcardPriceKopecks(t *testing.T) {
	mk := func(basic, product, total int64) wbSearchProduct {
		p := wbSearchProduct{}
		p.Sizes = append(p.Sizes, struct {
			Price struct {
				Basic   int64 `json:"basic"`
				Product int64 `json:"product"`
				Total   int64 `json:"total"`
			} `json:"price"`
		}{})
		p.Sizes[0].Price.Basic, p.Sizes[0].Price.Product, p.Sizes[0].Price.Total = basic, product, total
		return p
	}
	cases := []struct {
		name string
		p    wbSearchProduct
		want int64
	}{
		{"product приоритетнее", mk(143900, 79800, 0), 79800},
		{"фолбэк на total", mk(143900, 0, 80000), 80000},
		{"фолбэк на basic", mk(143900, 0, 0), 143900},
		{"нет размеров → 0 (нет оффера)", wbSearchProduct{}, 0},
	}
	for _, c := range cases {
		if got := ucardPriceKopecks(c.p); got != c.want {
			t.Errorf("%s: ucardPriceKopecks = %d, want %d", c.name, got, c.want)
		}
	}
}

// Реальная форма ответа u-card v4 (трансграничный товар «Находки из Китая»).
func TestUcardResponseShape(t *testing.T) {
	const sample = `{"products":[{"id":1114323146,"name":"Мужские футболки Gildan","brand":"JXKHOMN",
	  "sizes":[{"price":{"basic":87600,"product":79800,"total":0}}]}]}`
	var parsed wbSearchResponse
	if err := json.Unmarshal([]byte(sample), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Products) != 1 {
		t.Fatalf("products = %d, want 1", len(parsed.Products))
	}
	if got := ucardPriceKopecks(parsed.Products[0]); got != 79800 {
		t.Errorf("price = %d, want 79800 (product)", got)
	}
	if parsed.Products[0].Name == "" {
		t.Error("имя не распарсилось")
	}
}
