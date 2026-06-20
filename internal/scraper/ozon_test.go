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
		"1 299 ₽":   1299, // обычный пробел
		"1 299 ₽":   1299, // неразрывный пробел (как у Ozon)
		"1 299 ₽":   1299, // узкий пробел
		"999 ₽":     999,
		"":          0,
		"бесплатно": 0,
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
			"webGallery-5678-default-1": "{\"coverImage\":\"https://ir.ozone.ru/s3/multimedia-1-c/wc1000/7012345678.jpg\",\"images\":[{\"src\":\"https://ir.ozone.ru/s3/multimedia-1-d/wc1000/7012345679.jpg\"}]}"
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
	if res.ImageURL != "https://ir.ozone.ru/s3/multimedia-1-c/wc1000/7012345678.jpg" {
		t.Errorf("ImageURL = %q; want coverImage multimedia URL", res.ImageURL)
	}
}

func TestParseOzonWidgetsMobilePrice(t *testing.T) {
	// Реальная структура мобильного composer-api: виджет price-… с массивом
	// price.price[] из {text, textStyle}. Берём PRICE (текущую), не ORIGINAL_PRICE.
	body := []byte(`{
		"widgetStates": {
			"price-11475853-pdppage2copy-1": "{\"price\":{\"price\":[{\"text\":\"202 ₽\",\"textStyle\":\"PRICE\"},{\"text\":\"798 ₽\",\"textStyle\":\"ORIGINAL_PRICE\"}],\"discount\":\"−74%\"}}",
			"navTitle-4572512-pdppage2copy-1": "{\"title\":\"Подставка для телефона\"}",
			"galleryPreview-7889442-pdppage2copy-1": "{\"coverImage\":\"https://ir.ozone.ru/s3/x.jpg\"}"
		}
	}`)
	res, err := parseOzonWidgets(body)
	if err != nil {
		t.Fatalf("parseOzonWidgets error: %v", err)
	}
	if res.Price != 202 {
		t.Errorf("Price = %v; want 202 (PRICE, not ORIGINAL_PRICE)", res.Price)
	}
	if res.Name != "Подставка для телефона" {
		t.Errorf("Name = %q; want product title", res.Name)
	}
}

// TestExtractOzonNameDeterministic проверяет, что заголовок выбирается стабильно:
//   - всегда выигрывает выделенный заголовочный виджет (navTitle), а не посторонние
//     title-несущие виджеты (секция «с этим покупают», табы);
//   - shallowest-first берёт собственный title виджета, а не вложенные хлебные крошки;
//   - результат не «прыгает» от случайного порядка map (range по map рандомизирован) —
//     поэтому гоняем много раз и требуем одинаковый ответ.
func TestExtractOzonNameDeterministic(t *testing.T) {
	ws := map[string]string{
		// выделенный заголовок товара + вложенные хлебные крошки (не должны победить)
		"navTitle-100-pdppage2copy-1": `{"title":"Правильное имя товара",` +
			`"breadCrumbs":[{"title":"Каталог"},{"title":"Телефоны"}]}`,
		// посторонние виджеты с title в имени/значении — не должны перебить заголовок
		"webSectionTitle-200-default-1": `{"title":"С этим товаром покупают"}`,
		"cellList-300-default-1":        `{"text":"Похожие товары"}`,
	}
	const want = "Правильное имя товара"
	for i := 0; i < 100; i++ {
		if got := extractOzonName(ws); got != want {
			t.Fatalf("extractOzonName (итерация %d) = %q; want %q — нестабильный/неверный выбор", i, got, want)
		}
	}
}

// TestExtractOzonNameFallbackTier: если выделенного заголовочного виджета нет, берём
// фолбэком любой *title*-виджет (детерминированно, по сортировке имени).
func TestExtractOzonNameFallbackTier(t *testing.T) {
	ws := map[string]string{
		"webSomeTitle-9-default-1": `{"title":"Запасной заголовок"}`,
	}
	if got := extractOzonName(ws); got != "Запасной заголовок" {
		t.Errorf("extractOzonName fallback = %q; want %q", got, "Запасной заголовок")
	}
}

// TestOzonGatesNoFalsePositive: реальная карточка БЕЗ цены не должна классиф-ся как
// гейт 18+/login из-за слов-ловушек в глобальном меню. На реальном ответе Ozon
// (20.06) catalogMenu всегда несёт «Товары для взрослых»/adult, horizontalMenu —
// /fintech/signin; раньше это давало ложный ErrAgeRestricted при любом price==0.
func TestOzonGatesNoFalsePositive(t *testing.T) {
	body := []byte(`{"widgetStates":{
		"catalogMenu-7278490-default-1":"{\"items\":[{\"id\":\"9000\",\"title\":\"Товары для взрослых\",\"url\":\"/category/tovary-dlya-vzroslyh-9000/\",\"image\":\"https://ir.ozone.ru/s3/searchteam-cdn/adult_products_9000.png\",\"icon\":\"ic_m_adult_content_filled\"}]}",
		"horizontalMenu-7302642-default-1":"{\"items\":[{\"title\":\"Ozon Банк\",\"link\":\"https://ozon.ru/fintech/signin\"}]}",
		"webProductHeading-3385933-default-1":"{\"title\":\"Бейсболка\"}",
		"webDetailSKU-3385551-default-1":"{\"sku\":1551884914}"
	}}`)
	_, err := parseOzonWidgets(body)
	if err == ErrAgeRestricted || err == ErrAuthExpired {
		t.Fatalf("ложный гейт на обычной карточке без цены: %v", err)
	}
	if err == nil {
		t.Fatal("ожидалась ошибка (цены нет), got nil")
	}
}

// TestOzonAgeGateReal: настоящий 18+ гейт (карточки нет, есть age-виджет) → ErrAgeRestricted.
func TestOzonAgeGateReal(t *testing.T) {
	body := []byte(`{"widgetStates":{
		"catalogMenu-7278490-default-1":"{\"items\":[{\"title\":\"Товары для взрослых\"}]}",
		"adultModal-1-default-1":"{\"title\":\"Вам уже есть 18 лет?\",\"text\":\"Подтвердите возраст\"}"
	}}`)
	if _, err := parseOzonWidgets(body); err != ErrAgeRestricted {
		t.Fatalf("ожидался ErrAgeRestricted; got %v", err)
	}
}

// TestOzonLoginGateReal: протухшая сессия (карточки нет, страница логина) → ErrAuthExpired.
func TestOzonLoginGateReal(t *testing.T) {
	body := []byte(`{"widgetStates":{
		"catalogMenu-7278490-default-1":"{\"items\":[{\"title\":\"Товары для взрослых\"}]}",
		"loginForm-1-default-1":"{\"title\":\"Войдите в Ozon\"}"
	}}`)
	if _, err := parseOzonWidgets(body); err != ErrAuthExpired {
		t.Fatalf("ожидался ErrAuthExpired; got %v", err)
	}
}

func TestParseOzonWidgetsNoPrice(t *testing.T) {
	body := []byte(`{"widgetStates":{"webProductHeading-1":"{\"title\":\"X\"}"}}`)
	if _, err := parseOzonWidgets(body); err == nil {
		t.Error("expected error when price widget missing")
	}
}
