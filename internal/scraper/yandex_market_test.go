package scraper

import (
	"errors"
	"testing"
)

func TestParseYandexMarketHTML(t *testing.T) {
	html := `<html><head>
<script type="application/ld+json">{"@type":"BreadcrumbList","name":"x"}</script>
<script type="application/ld+json">
{"@type":"Product","name":"Смартфон Apple iPhone 15","image":["https://avatars.mds.yandex.net/get-mpic/1/img.jpg","https://avatars.mds.yandex.net/get-mpic/2/img.jpg"],"offers":{"price":"79990.00"}}
</script>
</head><body>...</body></html>`

	r, err := parseYandexMarketHTML(html)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Name != "Смартфон Apple iPhone 15" {
		t.Errorf("name = %q", r.Name)
	}
	if r.Price != 79990 {
		t.Errorf("price = %v, want 79990", r.Price)
	}
	if r.ImageURL != "https://avatars.mds.yandex.net/get-mpic/1/img.jpg" {
		t.Errorf("image = %q", r.ImageURL)
	}
}

func TestParseYandexMarketHTML_StringImage(t *testing.T) {
	// image как строка (не массив) — JSON-LD отдаёт оба варианта.
	html := `<script type="application/ld+json">{"@type":"Product","name":"X","image":"https://im.jpg","offers":{"price":"100"}}</script>`
	r, err := parseYandexMarketHTML(html)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.ImageURL != "https://im.jpg" || r.Price != 100 {
		t.Errorf("got %+v", r)
	}
}

func TestParseYandexMarketHTML_NoProduct(t *testing.T) {
	html := `<script type="application/ld+json">{"@type":"WebSite","name":"Я.Маркет"}</script>`
	if _, err := parseYandexMarketHTML(html); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("want ErrProductNotFound, got %v", err)
	}
}

func TestIsYandexCaptcha(t *testing.T) {
	if !isYandexCaptcha([]byte(`<div class="SmartCaptcha">...`)) {
		t.Error("should detect SmartCaptcha")
	}
	if isYandexCaptcha([]byte(`<script type="application/ld+json">{"@type":"Product"}</script>`)) {
		t.Error("false positive on product page")
	}
}

func TestYandexMatches(t *testing.T) {
	s := &YandexMarketScraper{}
	if !s.Matches("https://market.yandex.ru/product--slug/123") {
		t.Error("should match yandex market url")
	}
	if s.Matches("https://www.ozon.ru/product/123") {
		t.Error("should not match ozon url")
	}
}
