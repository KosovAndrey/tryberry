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

// Срез реального ответа /aer-webapi/v1/search (один товар).
const aliSearchSample = `{"data":{"productsFeed":{"productsV2":[
  {"snippetContainer":{
    "itemData":{
      "pdpInfo":{"preloadedData":{"title":"Женская Приталенная Футболка","price":{"value":281,"currency":"RUB"}},"url":"https://aliexpress.ru/item/1005009686147003.html?sku_id=12000049857067681"},
      "properties":{"id":"1005009686147003","index":"0"},
      "trackingInfo":{"webTrackInfo":{"aerEvent":{"itemId":"1005009686147003","finalPrice":281,"price":864,"soldCount":14191}}}
    },
    "presentations":[{"layoutAreas":[{"layers":[{"container":[{"elements":[{"gallery":{"images":["https://ae-pic-a1.aliexpress-media.com/kf/S593c49ac.jpg_480x480.jpg","https://ae-pic-a1.aliexpress-media.com/kf/Sb3a1ec9f.jpg_480x480.jpg"]}}]}]}]}]}]
  }}
]}}}`

func TestParseAliexpressSearch(t *testing.T) {
	items, err := parseAliexpressSearch([]byte(aliSearchSample))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("got %d items, want 1", len(items))
	}
	it := items[0]
	if it.ArticleID != "1005009686147003" {
		t.Errorf("ArticleID = %q", it.ArticleID)
	}
	if it.Name != "Женская Приталенная Футболка" {
		t.Errorf("Name = %q", it.Name)
	}
	if it.URL != "https://aliexpress.ru/item/1005009686147003.html" {
		t.Errorf("URL = %q", it.URL)
	}
	if it.PriceKopecks != 28100 {
		t.Errorf("PriceKopecks = %d, want 28100", it.PriceKopecks)
	}
	if it.OldPriceKopecks != 86400 {
		t.Errorf("OldPriceKopecks = %d, want 86400", it.OldPriceKopecks)
	}
	if it.ImageURL != "https://ae-pic-a1.aliexpress-media.com/kf/S593c49ac.jpg_480x480.jpg" {
		t.Errorf("ImageURL = %q", it.ImageURL)
	}
}
