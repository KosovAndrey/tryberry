package scraper

import (
	"errors"
	"net/url"
	"testing"
)

func newYMSearch() *YandexMarketSearchScraper {
	// base без прокси: client поднимется (tls-client не требует прокси для init),
	// но сетевых вызовов в этих тестах нет — проверяем только разбор URL/стейта.
	return NewYandexMarketSearchScraper(NewYandexMarketScraper(YandexMarketOptions{}), 60)
}

func TestYandexSearch_MatchesSearch(t *testing.T) {
	s := newYMSearch()
	cases := map[string]bool{
		"https://market.yandex.ru/search?text=кофемашина":           true,
		"https://market.yandex.ru/search?text=кофемашина&hid=90555": true,
		"https://market.yandex.ru/card/kofemashina-jura/5193397317": false, // карточка
		"https://www.wildberries.ru/catalog/0/search.aspx?search=x": false, // другой МП
		"https://market.yandex.ru/product--slug/123":                false, // карточка
	}
	for url, want := range cases {
		if got := s.MatchesSearch(url); got != want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", url, got, want)
		}
	}
}

func TestYandexSearch_NormalizeSearchURL(t *testing.T) {
	s := newYMSearch()
	// text нормализуется (lower + схлопывание пробелов), hid сохраняется. Сравниваем
	// по разобранным параметрам — Encode() percent-кодирует кириллицу.
	got, err := s.NormalizeSearchURL("https://market.yandex.ru/search?text=Кофемашина%20%20JURA&hid=90555&foo=bar")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	u, _ := url.Parse(got)
	if u.Scheme+"://"+u.Host+u.Path != "https://market.yandex.ru/search" {
		t.Errorf("base = %q", u.Scheme+"://"+u.Host+u.Path)
	}
	if q := u.Query(); q.Get("text") != "кофемашина jura" || q.Get("hid") != "90555" || q.Get("foo") != "" {
		t.Errorf("query = %v, want text='кофемашина jura' hid=90555 без foo", u.Query())
	}

	// Без text — ErrInvalidURL (нельзя дедуплицировать).
	if _, err := s.NormalizeSearchURL("https://market.yandex.ru/search?hid=90555"); !errors.Is(err, ErrInvalidURL) {
		t.Errorf("want ErrInvalidURL for URL without text, got %v", err)
	}
}

func TestYandexSearch_parseSearchPrices(t *testing.T) {
	// FIRST-PASS: вытаскиваем цены из сниппетов стейта (имя/URL — TODO по проду).
	html := `garbage "price":{"value":"25997","currency":"RUR"} more ` +
		`"price":{"value":"128931","currency":"RUR"} tail`
	s := newYMSearch()
	out := s.parseSearch(html)
	if len(out.Items) != 2 {
		t.Fatalf("got %d items, want 2", len(out.Items))
	}
	if out.Items[0].PriceKopecks != 25997*100 || out.Items[1].PriceKopecks != 128931*100 {
		t.Errorf("prices = %d, %d kopecks", out.Items[0].PriceKopecks, out.Items[1].PriceKopecks)
	}
}

func TestOzonSearch_URLHandling(t *testing.T) {
	s := NewOzonSearchScraper(NewOzonScraper(OzonOptions{}))
	if !s.MatchesSearch("https://www.ozon.ru/search/?text=наушники") {
		t.Error("should match ozon search URL")
	}
	if s.MatchesSearch("https://www.ozon.ru/product/foo-123/") {
		t.Error("should not match ozon product URL")
	}
	got, err := s.NormalizeSearchURL("https://www.ozon.ru/search/?text=Наушники&sorting=price")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	ou, _ := url.Parse(got)
	if ou.Path != "/search/" || ou.Query().Get("text") != "наушники" || ou.Query().Get("sorting") != "price" {
		t.Errorf("normalize = %q (query %v)", got, ou.Query())
	}
	// ScrapeSearch пока заблокирован (FAB + сайдкар без search).
	if _, err := s.ScrapeSearch(nil, "https://www.ozon.ru/search/?text=x"); !errors.Is(err, ErrMarketplaceBlocked) {
		t.Errorf("want ErrMarketplaceBlocked, got %v", err)
	}
}
