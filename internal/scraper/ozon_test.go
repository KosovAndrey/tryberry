package scraper

import (
	"strings"
	"testing"
)

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
	res, err := parseOzonWidgets(body, "1")
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
	res, err := parseOzonWidgets(body, "1")
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
	_, err := parseOzonWidgets(body, "1")
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
	if _, err := parseOzonWidgets(body, "1"); err != ErrAgeRestricted {
		t.Fatalf("ожидался ErrAgeRestricted; got %v", err)
	}
}

// TestOzonAgeGateAnonLive: ТОЧНАЯ сигнатура анонимного 18+ гейта, снятая вживую
// (probe_dump, нож 1729108994, 2026-07-02): ответ 200, ~9КБ, ЕДИНСТВЕННЫЙ виджет
// userAdultModal (ни карточки, ни меню). Ключ содержит "adult" → ErrAgeRestricted.
// Это тот случай, на котором гибрид уходит на authed-дорожку.
func TestOzonAgeGateAnonLive(t *testing.T) {
	body := []byte(`{"widgetStates":{
		"userAdultModal-747789-default-1":"{\"title\":{\"text\":\"Подтвердите возраст\",\"textStyle\":\"tsHeadL\"},\"subtitle\":{\"text\":\"Данный раздел предназначен только для лиц, достигших 18 лет.\"}}"
	}}`)
	if _, err := parseOzonWidgets(body, "1"); err != ErrAgeRestricted {
		t.Fatalf("ожидался ErrAgeRestricted на живой сигнатуре userAdultModal; got %v", err)
	}
}

// TestOzonLoginGateReal: протухшая сессия (карточки нет, страница логина) → ErrAuthExpired.
func TestOzonLoginGateReal(t *testing.T) {
	body := []byte(`{"widgetStates":{
		"catalogMenu-7278490-default-1":"{\"items\":[{\"title\":\"Товары для взрослых\"}]}",
		"loginForm-1-default-1":"{\"title\":\"Войдите в Ozon\"}"
	}}`)
	if _, err := parseOzonWidgets(body, "1"); err != ErrAuthExpired {
		t.Fatalf("ожидался ErrAuthExpired; got %v", err)
	}
}

func TestParseOzonWidgetsNoPrice(t *testing.T) {
	body := []byte(`{"widgetStates":{"webProductHeading-1":"{\"title\":\"X\"}"}}`)
	if _, err := parseOzonWidgets(body, "1"); err == nil {
		t.Error("expected error when price widget missing")
	}
}

// ozonOOSBody — УРЕЗАННАЯ РЕАЛЬНАЯ сигнатура страницы товара не в продаже
// (дамп 2274265393 = Palit RTX 5070 Ti, 2026-07-15): своего ценового и
// галерейного виджетов нет, есть webOutOfStock с нашим sku + полка «с этим
// покупают» skuShelfGoods с ЧУЖИМ товаром за 2 211 ₽ и его фото.
const ozonOOSBody = `{"widgetStates":{
	"webOutOfStock-1832453-default-1":"{\"sku\":\"2274265393\",\"skuName\":\"Palit Видеокарта GeForce RTX 5070 Ti GamingPro-S OC 16 ГБ (NE7507TS19T2-GB2031U)\",\"productLink\":\"/product/2274265393/?oos_search=false\",\"coverImage\":\"https://ir.ozone.ru/s3/multimedia-1-9/c200/7613089461.jpg\",\"price\":\"90 617 ₽\",\"deliveryMessage\":\"Доставка недоступна\",\"lexemes\":{\"outOfStockTitle\":\"Этот товар закончился\"}}",
	"skuShelfGoods-7138457-default-1":"{\"skuId\":\"2014279173\",\"state\":[{\"type\":\"priceV2\",\"priceV2\":{\"price\":[{\"text\":\"2 211 ₽\",\"textStyle\":\"PRICE\"},{\"text\":\"3 720 ₽\",\"textStyle\":\"ORIGINAL_PRICE\"}]}},{\"type\":\"image\",\"image\":{\"link\":\"https://ir.ozone.ru/s3/multimedia-1-z/11518384895.jpg\"}}]}",
	"catalogMenu-7278490-default-1":"{\"items\":[{\"title\":\"Цены от 39 ₽\",\"image\":\"https://ir.ozone.ru/s3/multimedia-1-z/7354360871.jpg\"}]}"
}}`

// TestParseOzonWidgetsOutOfStock — регресс на боевой баг: OOS-товар отдавал цену и
// фото ЧУЖОГО товара с полки «с этим покупают» (2 211 ₽ вместо 90 617 ₽, чужая
// картинка), при этом InStock был захардкожен true → публичная страница /p/
// показывала «Сейчас выгодно» на выдуманную цену, а мусор оседал в price_history.
func TestParseOzonWidgetsOutOfStock(t *testing.T) {
	res, err := parseOzonWidgets([]byte(ozonOOSBody), "2274265393")
	if err != nil {
		t.Fatalf("parseOzonWidgets error: %v", err)
	}
	if res.InStock {
		t.Error("InStock = true; товара нет в продаже (webOutOfStock)")
	}
	if res.Price != 90617 {
		t.Errorf("Price = %v; want 90617 (last-known из webOutOfStock, НЕ 2211 с полки)", res.Price)
	}
	if !strings.Contains(res.Name, "Palit") {
		t.Errorf("Name = %q; want имя нашего товара из skuName", res.Name)
	}
	if res.ImageURL != "https://ir.ozone.ru/s3/multimedia-1-9/c200/7613089461.jpg" {
		t.Errorf("ImageURL = %q; want coverImage нашей карточки", res.ImageURL)
	}
}

// ozonOOSBodyV2 — новая вложенная сигнатура webOutOfStock (дамп 2230185975,
// 2026-09-28): без поля sku, id только в common.action.link. Плюс полка
// tileGridDesktop с чужим товаром.
const ozonOOSBodyV2 = `{"widgetStates":{
	"webOutOfStock-13545189-default-1":"{\"text\":{\"text\":\"Генератор инверторный BOXBOT BGI-5000E, 5 кВт\",\"textColor\":\"textPrimary\"},\"image\":{\"image\":\"https://ir.ozone.ru/s3/multimedia-1-a/7590938590.jpg\",\"aspectRatio\":\"RATIO_3_4\"},\"price\":{\"price\":[{\"text\":\"27 890 ₽\",\"textStyle\":\"PRICE\",\"color\":\"textTertiary\"}],\"priceStyle\":{\"styleType\":\"UNAVAILABLE\"}},\"common\":{\"action\":{\"behavior\":\"BEHAVIOR_TYPE_REDIRECT\",\"link\":\"/product/2230185975/?oos_search=false\"}}}",
	"tileGridDesktop-3669724-default-1":"{\"items\":[{\"sku\":4855158561,\"mainState\":[{\"type\":\"priceV2\",\"priceV2\":{\"price\":[{\"text\":\"19 990 ₽\",\"textStyle\":\"PRICE\"}]}}]}]}"
}}`

func TestParseOzonWidgetsOutOfStockV2(t *testing.T) {
	res, err := parseOzonWidgets([]byte(ozonOOSBodyV2), "2230185975")
	if err != nil {
		t.Fatalf("parseOzonWidgets error: %v", err)
	}
	if res.InStock {
		t.Error("InStock = true; товара нет в продаже")
	}
	if res.Price != 27890 {
		t.Errorf("Price = %v; want 27890", res.Price)
	}
	if !strings.Contains(res.Name, "BOXBOT") {
		t.Errorf("Name = %q", res.Name)
	}
	if res.ImageURL != "https://ir.ozone.ru/s3/multimedia-1-a/7590938590.jpg" {
		t.Errorf("ImageURL = %q", res.ImageURL)
	}
	if _, err := parseOzonWidgets([]byte(ozonOOSBodyV2), "999"); err == nil {
		t.Error("чужой id в ссылке виджета не должен давать результат")
	}
}

// TestParseOzonWidgetsOutOfStockForeignSKU: OOS-виджет, подписанный ЧУЖИМ sku
// (предложение другого продавца / полка), не должен выдаваться за наш товар.
func TestParseOzonWidgetsOutOfStockForeignSKU(t *testing.T) {
	res, err := parseOzonWidgets([]byte(ozonOOSBody), "999999")
	if err == nil {
		t.Fatalf("ожидалась ошибка (своей карточки в ответе нет), got %+v", res)
	}
	if res != nil {
		t.Errorf("res = %+v; чужой OOS-виджет не должен давать результат", res)
	}
}

// TestExtractOzonPriceIgnoresForeignWidgets: цену берём ТОЛЬКО из ценового виджета
// карточки. Полки/меню/фильтры несут ₽ — раньше ярус «любой виджет с ₽» хватал их.
func TestExtractOzonPriceIgnoresForeignWidgets(t *testing.T) {
	ws := map[string]string{
		"skuShelfGoods-7138457-default-1":   `{"skuId":"2014279173","state":[{"type":"priceV2","priceV2":{"price":[{"text":"2 211 ₽","textStyle":"PRICE"}]}}]}`,
		"tileGridDesktop-3669724-default-1": `{"id":"2624124806","mainState":[{"type":"priceV2","priceV2":{"price":[{"text":"33 267 ₽","textStyle":"PRICE"}]}}]}`,
		"catalogMenu-7278490-default-1":     `{"items":[{"title":"Цены от 39 ₽"}]}`,
		"horizontalMenu-7302642-default-1":  `{"items":[{"title":"Товары за 1₽"}]}`,
	}
	if got := extractOzonPrice(ws); got != 0 {
		t.Errorf("extractOzonPrice = %v; want 0 — своего ценового виджета нет, чужие цены брать нельзя", got)
	}
}

// TestExtractOzonImageIgnoresForeignWidgets: фото берём только из галереи карточки.
// Раньше фолбэк шёл по всем виджетам в СЛУЧАЙНОМ порядке (range по map) — фото
// прыгало от скрейпа к скрейпу. Гоняем много раз: результат обязан быть стабилен.
func TestExtractOzonImageIgnoresForeignWidgets(t *testing.T) {
	ws := map[string]string{
		"skuShelfGoods-7138457-default-1": `{"items":[{"type":"image","image":{"link":"https://ir.ozone.ru/s3/multimedia-1-z/11518384895.jpg"}}]}`,
		"catalogMenu-7278490-default-1":   `{"items":[{"image":"https://ir.ozone.ru/s3/multimedia-1-z/7354360871.jpg"}]}`,
	}
	for i := 0; i < 100; i++ {
		if got := extractOzonImage(ws); got != "" {
			t.Fatalf("extractOzonImage (итерация %d) = %q; want пусто — галереи карточки нет", i, got)
		}
	}
}
