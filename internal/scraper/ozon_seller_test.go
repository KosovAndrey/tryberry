package scraper

import (
	"errors"
	"testing"
)

func newOzonSeller() *OzonSellerScraper {
	return NewOzonSellerScraper(NewOzonScraper(OzonOptions{Mode: "browser", BrowserURL: "http://x"}), 60)
}

func TestOzonSeller_MatchesSearch(t *testing.T) {
	s := newOzonSeller()
	cases := map[string]bool{
		"https://www.ozon.ru/seller/nike-store-12345/":          true,
		"https://www.ozon.ru/seller/onyx-156/?miniapp=seller_1": true,
		"https://www.ozon.ru/search/?text=кроссовки":            false, // выдача, не витрина
		"https://www.ozon.ru/product/foo-987/":                  false, // карточка
		"https://market.yandex.ru/business--x/1":                false, // другой МП
	}
	for u, want := range cases {
		if got := s.MatchesSearch(u); got != want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestOzonSeller_NormalizeAndName(t *testing.T) {
	s := newOzonSeller()
	got, err := s.NormalizeSearchURL("https://www.ozon.ru/seller/nike-store-12345/?miniapp=seller_1")
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if want := "https://www.ozon.ru/seller/nike-store-12345/"; got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}
	if _, err := s.NormalizeSearchURL("https://www.ozon.ru/search/?text=x"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("expected ErrInvalidURL for non-seller, got %v", err)
	}
	name, _ := s.SellerName(nil, "https://www.ozon.ru/seller/nike-store-12345/")
	if name != "Nike Store" {
		t.Errorf("SellerName = %q, want %q", name, "Nike Store")
	}
}
