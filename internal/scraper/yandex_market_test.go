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

func TestParseYandexMarketHTML_NumericPrice(t *testing.T) {
	// Цена числом (не строкой) — раньше роняла Unmarshal блока.
	html := `<script type="application/ld+json">{"@type":"Product","name":"X","offers":{"price":12990}}</script>`
	r, err := parseYandexMarketHTML(html)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 12990 {
		t.Errorf("price = %v, want 12990", r.Price)
	}
}

func TestParseYandexMarketHTML_AggregateOfferGraph(t *testing.T) {
	// @graph-обёртка + AggregateOffer с lowPrice + @type массивом.
	html := `<script type="application/ld+json">{"@context":"https://schema.org","@graph":[
		{"@type":"BreadcrumbList"},
		{"@type":["Product","IndividualProduct"],"name":"Y","image":"https://im.jpg",
		 "offers":{"@type":"AggregateOffer","lowPrice":3499,"priceCurrency":"RUR"}}
	]}</script>`
	r, err := parseYandexMarketHTML(html)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 3499 || r.Name != "Y" {
		t.Errorf("got %+v, want price 3499 name Y", r)
	}
}

func TestParseYandexMarketHTML_OffersArray(t *testing.T) {
	html := `<script type="application/ld+json">{"@type":"Product","name":"Z","offers":[{"price":"550"},{"price":"600"}]}</script>`
	r, err := parseYandexMarketHTML(html)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if r.Price != 550 {
		t.Errorf("price = %v, want 550 (first offer)", r.Price)
	}
}

func TestParseYandexMarketHTML_NoProduct(t *testing.T) {
	html := `<script type="application/ld+json">{"@type":"WebSite","name":"Я.Маркет"}</script>`
	if _, err := parseYandexMarketHTML(html); !errors.Is(err, ErrProductNotFound) {
		t.Errorf("want ErrProductNotFound, got %v", err)
	}
}

func TestIsYandexCaptcha(t *testing.T) {
	if !isYandexCaptcha([]byte(`<div class="SmartCaptcha">подтвердите, что запросы отправляли вы`)) {
		t.Error("should detect SmartCaptcha challenge page")
	}
	if isYandexCaptcha([]byte(`<script type="application/ld+json">{"@type":"Product"}</script>`)) {
		t.Error("false positive on product page")
	}
	// Реальная страница приложения Я.Маркета со словом captcha в бандле — НЕ блок
	// (это и был баг ложного "blocked").
	real := `<!DOCTYPE html><!--BEGIN [@marketfront/Root]--><html data-baobab-name="$page">` +
		`<script src="https://yastatic.net/captcha/captcha.js"></script>`
	if isYandexCaptcha([]byte(real)) {
		t.Error("false positive on real @marketfront page with captcha.js in bundle")
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
