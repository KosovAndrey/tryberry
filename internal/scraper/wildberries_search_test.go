package scraper

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"
)

// rtFunc — RoundTripper из функции (канонические ответы без сети).
type rtFunc func(*http.Request) *http.Response

func (f rtFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r), nil }

func stubResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(bytes.NewBufferString(body)),
		Header:     make(http.Header),
	}
}

// TestScrapeSearchFallbackToBrowser — direct отдаёт 403 (горячий запрос), сайдкар
// (browser) отдаёт 200: выдача собирается через сайдкар, direct не роняет запрос.
func TestScrapeSearchFallbackToBrowser(t *testing.T) {
	var directHits, sidecarHits int
	directClient := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		directHits++
		return stubResp(http.StatusForbidden, "blocked")
	})}
	sidecarClient := &http.Client{Transport: rtFunc(func(r *http.Request) *http.Response {
		sidecarHits++
		if got := r.URL.Query().Get("query"); got != "iphone" {
			t.Errorf("сайдкар получил query=%q, want iphone", got)
		}
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}

	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: directClient}}},
		tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
		maxPages:           1,
		browserURL:         "http://wb-search-miner:8081",
		browserClient:      sidecarClient,
		browserMaxPages:    1,
	}

	set, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone")
	if err != nil {
		t.Fatalf("ScrapeSearch с рабочим сайдкаром вернул ошибку: %v", err)
	}
	if len(set.Items) != 2 {
		t.Fatalf("собрано %d товаров, want 2 (через сайдкар)", len(set.Items))
	}
	if directHits == 0 || sidecarHits == 0 {
		t.Errorf("ожидались обращения и к direct (403), и к сайдкару (200): direct=%d sidecar=%d", directHits, sidecarHits)
	}
}

// TestScrapeSearchNoSidecarReturnsBlocked — без сайдкара 403 остаётся ошибкой.
func TestScrapeSearchNoSidecarReturnsBlocked(t *testing.T) {
	directClient := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		return stubResp(http.StatusForbidden, "blocked")
	})}
	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: directClient}}},
		tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
		maxPages:           1,
	}
	if _, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone"); err == nil {
		t.Fatal("ожидалась ошибка (403 без сайдкара), получили nil")
	}
}

func newTestSearchScraper() *WildberriesSearchScraper {
	return NewWildberriesSearchScraper(nil, nil, nil, 5, 0)
}

func TestMatchesSearch(t *testing.T) {
	s := newTestSearchScraper()
	cases := []struct {
		url  string
		want bool
	}{
		{"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone%2016", true},
		{"https://www.wildberries.ru/catalog/0/search.aspx?page=1&sort=popular&search=iphone+16", true},
		{"https://www.wildberries.ru/catalog/268509658/detail.aspx", false}, // карточка товара
		{"https://www.wildberries.ru/catalog/elektronika/smartfony", false}, // категория без search
		{"https://ozon.ru/search?text=iphone", false},                       // другой маркетплейс
		{"not a url at all", false},
	}
	for _, c := range cases {
		if got := s.MatchesSearch(c.url); got != c.want {
			t.Errorf("MatchesSearch(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestNormalizeSearchURL(t *testing.T) {
	s := newTestSearchScraper()

	// Разные по форме, но семантически одинаковые ссылки → один ключ.
	equivalent := []string{
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iPhone%2016",
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone+16&page=3&dest=-1257786&spp=30",
		"https://www.wildberries.ru/catalog/0/search.aspx?search=  iphone   16  &sort=popular",
	}
	var first string
	for i, u := range equivalent {
		got, err := s.NormalizeSearchURL(u)
		if err != nil {
			t.Fatalf("NormalizeSearchURL(%q) error: %v", u, err)
		}
		if i == 0 {
			first = got
			continue
		}
		if got != first {
			t.Errorf("normalized mismatch:\n  %q ->\n  %q\nwant\n  %q", u, got, first)
		}
	}

	// Разная сортировка → разные ключи.
	a, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&sort=popular")
	b, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&sort=pricedown")
	if a == b {
		t.Errorf("ожидались разные ключи для разных sort, оба = %q", a)
	}

	// Без текста запроса → ошибка.
	if _, err := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx"); err == nil {
		t.Error("ожидалась ошибка для URL без search=")
	}
}

func TestBuildSearchAPIURL(t *testing.T) {
	got := buildSearchAPIURL("", "iphone 16", "popular", 2, nil)
	for _, want := range []string{
		wbSearchAPIBase,
		"query=iphone+16",
		"page=2",
		"resultset=catalog",
		"sort=popular",
		"dest=-1257786",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("API URL %q не содержит %q", got, want)
		}
	}
}

// Ссылка с фильтрами WB (цена + предмет) — фильтры должны дойти и до ключа
// дедупа/кнопки «Открыть выдачу», и до запроса в u-search. Регрессия: без них
// «rtx 5080 от 13 437 ₽, предмет "видеокарты"» вырождался в голое «rtx 5080»
// и приносил наклейки с кулерами.
func TestSearchFiltersPreserved(t *testing.T) {
	s := &WildberriesSearchScraper{}
	raw := "https://www.wildberries.ru/catalog/0/search.aspx?page=1&sort=priceup&search=rtx+5080" +
		"&priceU=1343700%3B15000000&xsubject=3274&meta_charcs=false"

	norm, err := s.NormalizeSearchURL(raw)
	if err != nil {
		t.Fatalf("NormalizeSearchURL: %v", err)
	}
	for _, want := range []string{"search=rtx+5080", "sort=priceup", "priceU=1343700;15000000", "xsubject=3274"} {
		if !strings.Contains(norm, want) {
			t.Errorf("нормализованный URL %q не содержит %q", norm, want)
		}
	}
	if strings.Contains(norm, "meta_charcs") || strings.Contains(norm, "page=") {
		t.Errorf("в ключ дедупа попал не-фильтр: %q", norm)
	}

	// Нормализованный URL сам должен разбираться обратно (его же читает воркер).
	query, sortMode, filters, err := s.parseSearchParams(norm)
	if err != nil {
		t.Fatalf("парсинг нормализованного URL: %v", err)
	}
	if query != "rtx 5080" || sortMode != "priceup" {
		t.Errorf("query=%q sort=%q, want rtx 5080/priceup", query, sortMode)
	}
	if filters.Get("priceU") != "1343700;15000000" || filters.Get("xsubject") != "3274" {
		t.Errorf("фильтры после обратного разбора: %v", filters)
	}

	api := buildSearchAPIURL("", query, sortMode, 1, filters)
	if !strings.Contains(api, "priceU=1343700%3B15000000") || !strings.Contains(api, "xsubject=3274") {
		t.Errorf("фильтры не ушли в u-search: %q", api)
	}

	// Порядок параметров в ссылке не должен менять ключ дедупа.
	shuffled := "https://www.wildberries.ru/catalog/0/search.aspx?xsubject=3274&search=rtx+5080" +
		"&priceU=1343700;15000000&sort=priceup"
	if norm2, err := s.NormalizeSearchURL(shuffled); err != nil || norm2 != norm {
		t.Errorf("ключ зависит от порядка параметров:\n%q\n%q (err=%v)", norm, norm2, err)
	}

	// Разные фильтры → разные подписки; без фильтров → отдельный ключ.
	bare, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=rtx+5080&sort=priceup")
	if bare == norm {
		t.Error("выдача с фильтрами и без схлопнулись в один ключ")
	}
	other, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=rtx+5080&sort=priceup&xsubject=515")
	if other == norm {
		t.Error("разные значения xsubject дали один ключ")
	}
}

// Фасеты f<цифры> (бренд/характеристика) — тоже фильтры, значения внутри «;»
// сортируются, чтобы порядок галочек не плодил подписки.
func TestSearchFacetFiltersNormalized(t *testing.T) {
	s := &WildberriesSearchScraper{}
	a, err := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=кофе&f5023=b;a&fbrand=6049")
	if err != nil {
		t.Fatalf("NormalizeSearchURL: %v", err)
	}
	b, _ := s.NormalizeSearchURL("https://www.wildberries.ru/catalog/0/search.aspx?search=кофе&fbrand=6049&f5023=a;b")
	if a != b {
		t.Errorf("порядок значений фасета изменил ключ:\n%q\n%q", a, b)
	}
	if !strings.Contains(a, "f5023=a;b") || !strings.Contains(a, "fbrand=6049") {
		t.Errorf("фасеты потеряны: %q", a)
	}
}

// На 403 direct запрос уходит в сайдкар — фильтры должны уехать с ним, иначе
// браузер-дорожка принесёт голую выдачу.
func TestBrowserFallbackCarriesFilters(t *testing.T) {
	directClient := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		return stubResp(http.StatusForbidden, "blocked")
	})}
	var gotFilters string
	sidecarClient := &http.Client{Transport: rtFunc(func(r *http.Request) *http.Response {
		gotFilters = r.URL.Query().Get("filters")
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}
	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: directClient}}},
		tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
		maxPages:           1,
		browserURL:         "http://wb-search-miner:8081",
		browserClient:      sidecarClient,
		browserMaxPages:    1,
	}
	if _, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=rtx+5080&priceU=1343700;15000000&xsubject=3274"); err != nil {
		t.Fatalf("ScrapeSearch: %v", err)
	}
	if gotFilters != "priceU=1343700;15000000&xsubject=3274" {
		t.Errorf("сайдкар получил filters=%q", gotFilters)
	}
}

// Реальная форма ответа v18, снятая с сервера (цены в копейках, products в корне).
const sampleSearchJSON = `{
  "total": 1234,
  "products": [
    {"id":387704100,"name":"iPhone 16 256GB","brand":"Apple","feedbackPoints":0,
     "sizes":[{"price":{"basic":14199000,"product":6673500}}]},
    {"id":314263942,"name":"iPhone 16 128GB","brand":"Apple","feedbackPoints":900,
     "sizes":[{"price":{"basic":12999000,"product":5459500}}]},
    {"id":999999999,"name":"Нет в наличии","brand":"X",
     "sizes":[{"price":{"basic":0,"product":0}}]}
  ]
}`

func TestParseSearchResponse(t *testing.T) {
	var parsed wbSearchResponse
	if err := json.Unmarshal([]byte(sampleSearchJSON), &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Total != 1234 {
		t.Errorf("Total = %d, want 1234", parsed.Total)
	}
	if len(parsed.Products) != 3 {
		t.Fatalf("products = %d, want 3", len(parsed.Products))
	}

	// Первый товар: цена product, старая basic, баллов нет.
	it0 := wbProductToItem(parsed.Products[0], 1)
	if it0.PriceKopecks != 6673500 {
		t.Errorf("price = %d, want 6673500", it0.PriceKopecks)
	}
	if it0.OldPriceKopecks != 14199000 {
		t.Errorf("old price = %d, want 14199000", it0.OldPriceKopecks)
	}
	if it0.EffectivePriceKopecks() != 6673500 {
		t.Errorf("effective = %d, want 6673500 (баллов нет)", it0.EffectivePriceKopecks())
	}
	if it0.URL != "https://www.wildberries.ru/catalog/387704100/detail.aspx" {
		t.Errorf("url = %q", it0.URL)
	}
	if !strings.HasPrefix(it0.ImageURL, "https://basket-") || !strings.Contains(it0.ImageURL, "/387704100/") {
		t.Errorf("image url unexpected: %q", it0.ImageURL)
	}

	// Второй товар: 900 баллов → эффективная = product - 900*100.
	it1 := wbProductToItem(parsed.Products[1], 2)
	if !it1.HasFeedbackPoints() {
		t.Error("ожидались баллы у второго товара")
	}
	if want := int64(5459500 - 900*100); it1.EffectivePriceKopecks() != want {
		t.Errorf("effective = %d, want %d", it1.EffectivePriceKopecks(), want)
	}
}

func TestScrapeSearchSkipsZeroPrice(t *testing.T) {
	// Третий товар в сэмпле — нулевая цена; в выдачу попадать не должен.
	var parsed wbSearchResponse
	_ = json.Unmarshal([]byte(sampleSearchJSON), &parsed)

	kept := 0
	pos := 0
	for _, p := range parsed.Products {
		pos++
		if wbProductToItem(p, pos).PriceKopecks == 0 {
			continue
		}
		kept++
	}
	if kept != 2 {
		t.Errorf("оставлено %d товаров, want 2 (нулевая цена отброшена)", kept)
	}
}

func TestProxyPoolRoundRobin(t *testing.T) {
	pool, errs := NewProxyPool([]string{
		"http://u:p@proxy1.example:8080",
		"http://u:p@proxy2.example:8080",
		"  ",        // пустой — пропускается
		"://broken", // битый — в errs
	}, 5*time.Second)

	if len(errs) != 1 {
		t.Errorf("ожидалась 1 ошибка на битый прокси, got %d: %v", len(errs), errs)
	}
	if pool.Size() != 2 {
		t.Fatalf("pool size = %d, want 2", pool.Size())
	}
	if !pool.HasProxies() {
		t.Error("HasProxies() = false, want true")
	}

	// Round-robin: 4 вызова на 2 прокси → чередование a,b,a,b.
	seq := []string{pool.next().label, pool.next().label, pool.next().label, pool.next().label}
	if seq[0] == seq[1] {
		t.Errorf("ожидалось чередование прокси, got %v", seq)
	}
	if seq[0] != seq[2] || seq[1] != seq[3] {
		t.Errorf("ожидался период 2 в round-robin, got %v", seq)
	}
}

func TestProxyPoolEmptyIsDirect(t *testing.T) {
	pool, errs := NewProxyPool(nil, 0)
	if len(errs) != 0 {
		t.Errorf("неожиданные ошибки: %v", errs)
	}
	if pool.Size() != 1 {
		t.Errorf("pool size = %d, want 1 (direct)", pool.Size())
	}
	if pool.HasProxies() {
		t.Error("HasProxies() = true для пустого пула, want false")
	}
	if pool.next().label != "direct" {
		t.Errorf("label = %q, want direct", pool.next().label)
	}
}

// Ценовой фильтр досеивается на нашей стороне: u-search priceU игнорирует
// (замер 2026-08-24), поэтому товар вне диапазона обязан отсеяться у нас, иначе
// подписка «от 13 437 ₽» снова притащит наклейки по 700 ₽.
func TestSearchPriceRangeFilteredLocally(t *testing.T) {
	client := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}
	newScraper := func() *WildberriesSearchScraper {
		return &WildberriesSearchScraper{
			WildberriesScraper: NewWildberriesScraper(5),
			pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: client}}},
			tokens:             StaticTokenProvider{T: SearchToken{Cookie: "x_wbaas_token=abc", Slot: -1}},
			maxPages:           1,
		}
	}
	// В фикстуре два товара с ценой: 66 735 ₽ и 54 595 ₽ (в копейках).
	set, err := newScraper().ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&priceU=6000000;7000000")
	if err != nil {
		t.Fatalf("ScrapeSearch: %v", err)
	}
	if len(set.Items) != 1 || set.Items[0].PriceKopecks != 6673500 {
		t.Fatalf("после фильтра цены: %+v", set.Items)
	}
	if set.Items[0].Position != 1 {
		t.Errorf("позиция после отсева = %d, want 1 (нумерация без дыр)", set.Items[0].Position)
	}

	// Без фильтра — оба товара на месте (фильтр не должен «протекать»).
	all, err := newScraper().ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone")
	if err != nil {
		t.Fatalf("ScrapeSearch без фильтра: %v", err)
	}
	if len(all.Items) != 2 {
		t.Errorf("без фильтра собрано %d товаров, want 2", len(all.Items))
	}

	// Битые/перепутанные границы выдачу в ноль не режут.
	for _, bad := range []string{"priceU=abc", "priceU=15000000;1343700", "priceU=1343700"} {
		got, err := newScraper().ScrapeSearch(context.Background(),
			"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone&"+bad)
		if err != nil {
			t.Fatalf("ScrapeSearch %s: %v", bad, err)
		}
		if len(got.Items) != 2 {
			t.Errorf("%s: собрано %d, want 2 (границы не применяем)", bad, len(got.Items))
		}
	}
}

// Публичный search.wb.ru токена не требует: пустой пул токенов не должен
// валить запрос (замер 2026-08-24 — 200 без cookie). На same-origin проксике
// поведение прежнее: без токена запрос не уходит.
func TestPublicAPIBaseNeedsNoToken(t *testing.T) {
	var gotURL, gotCookie string
	client := &http.Client{Transport: rtFunc(func(r *http.Request) *http.Response {
		gotURL, gotCookie = r.URL.String(), r.Header.Get("Cookie")
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}
	newScraper := func() *WildberriesSearchScraper {
		return &WildberriesSearchScraper{
			WildberriesScraper: NewWildberriesScraper(5),
			pool:               &ProxyPool{clients: []proxyClient{{label: "direct", client: client}}},
			tokens:             StaticTokenProvider{}, // пул пуст — токена нет
			maxPages:           1,
		}
	}
	const publicBase = "https://search.wb.ru/exactmatch/ru/common/v18/search"

	s := newScraper()
	s.SetAPIBase(publicBase)
	set, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone")
	if err != nil {
		t.Fatalf("публичный хост без токена: %v", err)
	}
	if len(set.Items) != 2 {
		t.Errorf("собрано %d товаров, want 2", len(set.Items))
	}
	if !strings.HasPrefix(gotURL, publicBase+"?") {
		t.Errorf("запрос ушёл не на публичную базу: %q", gotURL)
	}
	if gotCookie != "" {
		t.Errorf("на публичный хост уехала cookie: %q", gotCookie)
	}

	// Без переключения базы — прежнее поведение: пустой токен блокирует запрос.
	if _, err := newScraper().ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone"); !errors.Is(err, ErrMarketplaceBlocked) {
		t.Errorf("на same-origin базе без токена ждём ErrMarketplaceBlocked, got %v", err)
	}
}

// spyTokenProvider — считает обращения к пулу токенов.
type spyTokenProvider struct {
	tok       SearchToken
	asked     int
	markedBad int
}

func (s *spyTokenProvider) Token(context.Context) (SearchToken, error) {
	s.asked++
	return s.tok, nil
}
func (s *spyTokenProvider) MarkBad(_ context.Context, slot int) {
	if slot >= 0 {
		s.markedBad++
	}
}
func (s *spyTokenProvider) MarkGood(context.Context, int) {}

// На публичном хосте 429 — это rate-limit, а не протухший токен: слот жечь
// нельзя (прод 24-08 успел пометить слот 4 битым на ровном месте), а пул
// вообще не должен опрашиваться — cookie wbaas чужому домену не нужна.
func TestPublicBase429DoesNotBurnTokenSlot(t *testing.T) {
	var calls int
	client := &http.Client{Transport: rtFunc(func(*http.Request) *http.Response {
		calls++
		if calls == 1 {
			return stubResp(http.StatusTooManyRequests, "slow down")
		}
		return stubResp(http.StatusOK, sampleSearchJSON)
	})}
	spy := &spyTokenProvider{tok: SearchToken{Cookie: "x_wbaas_token=abc", Slot: 4}}
	s := &WildberriesSearchScraper{
		WildberriesScraper: NewWildberriesScraper(5),
		pool:               &ProxyPool{clients: []proxyClient{{label: "xray", client: client}}},
		tokens:             spy,
		maxPages:           1,
	}
	s.SetAPIBase("https://search.wb.ru/exactmatch/ru/common/v18/search")

	set, err := s.ScrapeSearch(context.Background(),
		"https://www.wildberries.ru/catalog/0/search.aspx?search=iphone")
	if err != nil {
		t.Fatalf("после 429 ждём ретрай и успех, got %v", err)
	}
	if len(set.Items) != 2 {
		t.Errorf("собрано %d товаров, want 2", len(set.Items))
	}
	if spy.markedBad != 0 {
		t.Errorf("слот помечен битым %d раз(а) из-за rate-limit публичного хоста", spy.markedBad)
	}
	if spy.asked != 0 {
		t.Errorf("пул токенов опрошен %d раз(а) — публичному хосту токен не нужен", spy.asked)
	}
}

// Режим «сразу в браузер»: с 23-09-2026 публичная ручка закрыта насовсем, и
// поход в неё перед каждым запросом — это заведомый 403, который мы сами шлём
// в адрес WB. WB_SEARCH_DIRECT=false должен уводить запрос в сайдкар сразу.
func TestSearchDirectOffSkipsPublicHost(t *testing.T) {
	s := newTestSearchScraper()
	directCalls, browserCalls := 0, 0
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		directCalls++
		return resp(403, "", nil), nil
	})}
	s.SetBrowserSidecar("http://wb-search-miner:8081", 1)
	s.browserClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		browserCalls++
		return resp(200, `{"data":{"products":[]}}`, nil), nil
	})}
	s.SetSearchDirect(false)

	_, _ = s.ScrapeSearch(context.Background(), "https://www.wildberries.ru/catalog/0/search.aspx?search=rtx+5080")

	if directCalls != 0 {
		t.Fatalf("в закрытую публичную ручку сходили %d раз", directCalls)
	}
	if browserCalls == 0 {
		t.Fatal("сайдкар не позвали")
	}
}
