package scraper

import (
	"errors"
	"testing"
)

func TestAliexpressMatchesSearch(t *testing.T) {
	s := NewAliexpressSearchScraper(&AliexpressScraper{}, 60)
	cases := map[string]bool{
		"https://aliexpress.ru/wholesale?SearchText=футболка&page=1":     true,
		"https://aliexpress.ru/wholesale?SearchText=футболка&pvid=x&g=y": true,
		"https://aliexpress.ru/w/wholesale-naushniki.html":              true,
		"https://aliexpress.ru/wholesale?page=1":                        false, // нет SearchText
		"https://aliexpress.ru/item/1005005863682926.html":             false, // карточка
		"https://www.wildberries.ru/catalog/0/search.aspx?search=x":    false,
	}
	for u, want := range cases {
		if got := s.MatchesSearch(u); got != want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", u, got, want)
		}
	}
}

func TestAliexpressNormalizeSearchURL(t *testing.T) {
	s := NewAliexpressSearchScraper(&AliexpressScraper{}, 60)

	// Фильтр-токены и page отбрасываются, ключ = SearchText.
	got, err := s.NormalizeSearchURL("https://aliexpress.ru/wholesale?SearchText=Футболка&pvid=376-1723&searchInfo=abc&g=y&page=3")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	want := "https://aliexpress.ru/wholesale?SearchText=%D1%84%D1%83%D1%82%D0%B1%D0%BE%D0%BB%D0%BA%D0%B0"
	if got != want {
		t.Errorf("Normalize = %q, want %q", got, want)
	}

	// Тот же запрос с другими фильтрами → тот же ключ (схлопывание, см. коммент).
	got2, _ := s.NormalizeSearchURL("https://aliexpress.ru/wholesale?SearchText=футболка&pvid=999")
	if got2 != got {
		t.Errorf("expected same key, got %q vs %q", got2, got)
	}

	// Без SearchText → ErrInvalidURL.
	if _, err := s.NormalizeSearchURL("https://aliexpress.ru/wholesale?page=1"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("expected ErrInvalidURL, got %v", err)
	}
}
