package scraper

import (
	"errors"
	"testing"
)

func TestAliexpressMatches(t *testing.T) {
	s := &AliexpressScraper{}
	cases := map[string]bool{
		"https://aliexpress.ru/item/1005005863682926.html":      true,
		"https://aliexpress.ru/item/1005005863682926.html?spm=a": true,
		"https://www.aliexpress.com/item/1005006086965599.html":  true,
		"https://www.wildberries.ru/catalog/123/detail.aspx":     false,
		"https://www.ozon.ru/product/foo-123/":                   false,
	}
	for url, want := range cases {
		if got := s.Matches(url); got != want {
			t.Errorf("Matches(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestExtractAliexpressID(t *testing.T) {
	id, err := ExtractAliexpressID("https://aliexpress.ru/item/1005005863682926.html?spm=x")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if id != "1005005863682926" {
		t.Errorf("id = %q, want 1005005863682926", id)
	}
	if _, err := ExtractAliexpressID("https://aliexpress.ru/store/123"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("expected ErrInvalidURL, got %v", err)
	}
}

// Срез реального ответа aer-jsonapi productData (цена 128₽ активная, 255₽
// зачёркнутая, в наличии).
const aliProductDataSample = `{"data":{
  "id":"1005005863682926",
  "name":"Детские гольфы ZDOMAIN хлопок",
  "gallery":[{"imageUrl":"https://ae-pic-a1.aliexpress-media.com/kf/S458.jpg"}],
  "price":{
    "discount":50,
    "minActivityAmount":{"value":128,"currency":"RUB","formatted":"128 ₽"},
    "minAmount":{"value":255,"currency":"RUB","formatted":"255 ₽"}
  },
  "preselectSkuOutOfStockInfo":null,
  "analytics":{"viewProduct":{"trackingInfo":{"available":true,"finalPrice":128}}}
}}`

func TestParseAliexpressProductData(t *testing.T) {
	r, err := parseAliexpressProductData([]byte(aliProductDataSample), "https://aliexpress.ru/item/1005005863682926.html")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if r.Price != 128 {
		t.Errorf("Price = %v, want 128 (активная цена)", r.Price)
	}
	if r.Name != "Детские гольфы ZDOMAIN хлопок" {
		t.Errorf("Name = %q", r.Name)
	}
	if !r.InStock {
		t.Error("InStock = false, want true")
	}
	if r.ImageURL == "" {
		t.Error("ImageURL пуст")
	}
}

func TestParseAliexpressFallbackToAmount(t *testing.T) {
	// Нет активной цены → берём зачёркнутую (minAmount).
	body := `{"data":{"name":"X","price":{"minAmount":{"value":300,"currency":"RUB"}},
	  "analytics":{"viewProduct":{"trackingInfo":{"available":true}}}}}`
	r, err := parseAliexpressProductData([]byte(body), "u")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.Price != 300 {
		t.Errorf("Price = %v, want 300", r.Price)
	}
}

func TestParseAliexpressOutOfStock(t *testing.T) {
	// Нет цены + OOS-блок → InStock=false, Price=0, без ошибки.
	body := `{"data":{"name":"X","price":{},"preselectSkuOutOfStockInfo":{"text":"нет"},
	  "analytics":{"viewProduct":{"trackingInfo":{"available":false}}}}}`
	r, err := parseAliexpressProductData([]byte(body), "u")
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if r.InStock {
		t.Error("InStock = true, want false")
	}
	if r.Price != 0 {
		t.Errorf("Price = %v, want 0", r.Price)
	}
}

func TestAliexpressScrapeNotConfigured(t *testing.T) {
	s := &AliexpressScraper{} // configured=false
	if _, err := s.Scrape(nil, "https://aliexpress.ru/item/123.html"); !errors.Is(err, ErrNotImplemented) {
		t.Errorf("expected ErrNotImplemented, got %v", err)
	}
}
