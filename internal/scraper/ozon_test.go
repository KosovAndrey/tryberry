package scraper

import "testing"

func TestExtractOzonID(t *testing.T) {
	cases := []struct {
		url  string
		want string
		ok   bool
	}{
		{"https://www.ozon.ru/product/nazvanie-tovara-123456789/", "123456789", true},
		{"https://www.ozon.ru/product/123456789/", "123456789", true},
		{"https://www.ozon.ru/product/smartfon-apple-iphone-15-256gb-1693565891/?asb=abc", "1693565891", true},
		{"https://ozon.ru/product/test-987654321", "987654321", true},
		{"https://www.wildberries.ru/catalog/123/detail.aspx", "", false},
		{"https://www.ozon.ru/category/smartfony-15502/", "", false},
	}
	for _, c := range cases {
		got, err := extractOzonID(c.url)
		if c.ok && (err != nil || got != c.want) {
			t.Errorf("extractOzonID(%q) = %q, %v; want %q, nil", c.url, got, err, c.want)
		}
		if !c.ok && err == nil {
			t.Errorf("extractOzonID(%q) = %q, nil; want error", c.url, got)
		}
	}
}

func TestParseRubles(t *testing.T) {
	cases := map[string]float64{
		"1 299 ₽":       1299, // обычный пробел
		"1 299 ₽": 1299, // неразрывный пробел (как у Ozon)
		"1 299 ₽":  1299, // узкий пробел
		"999 ₽":         999,
		"":              0,
		"бесплатно":     0,
	}
	for in, want := range cases {
		if got := parseRubles(in); got != want {
			t.Errorf("parseRubles(%q) = %v; want %v", in, got, want)
		}
	}
}

func TestParseOzonWidgets(t *testing.T) {
	// Синтетический ответ entrypoint-api: widgetStates с JSON-строками внутри.
	body := []byte(`{
		"widgetStates": {
			"webPrice-3132441-default-1": "{\"price\":\"1 299 ₽\",\"originalPrice\":\"2 000 ₽\",\"cardPrice\":\"1 199 ₽\"}",
			"webProductHeading-1234-default-1": "{\"title\":\"Смартфон Apple iPhone 15\"}",
			"webGallery-5678-default-1": "{\"coverImage\":\"https://cdn1.ozone.ru/s3/x.jpg\",\"images\":[{\"src\":\"https://cdn1.ozone.ru/s3/1.jpg\"}]}"
		}
	}`)
	res, err := parseOzonWidgets(body)
	if err != nil {
		t.Fatalf("parseOzonWidgets error: %v", err)
	}
	if res.Price != 1299 {
		t.Errorf("Price = %v; want 1299", res.Price)
	}
	if res.Name != "Смартфон Apple iPhone 15" {
		t.Errorf("Name = %q; want product title", res.Name)
	}
	if res.ImageURL != "https://cdn1.ozone.ru/s3/x.jpg" {
		t.Errorf("ImageURL = %q; want coverImage", res.ImageURL)
	}
}

func TestParseOzonWidgetsNoPrice(t *testing.T) {
	body := []byte(`{"widgetStates":{"webProductHeading-1":"{\"title\":\"X\"}"}}`)
	if _, err := parseOzonWidgets(body); err == nil {
		t.Error("expected error when price widget missing")
	}
}
