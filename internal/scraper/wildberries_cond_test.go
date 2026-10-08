package scraper

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

// Тесты дешёвого change-detection basket-CDN: conditional GET price-history.json
// по валидаторам прошлого скрейпа. 304 → Result восстанавливается из снимка
// (CondEntry), card.json не запрашивается. HTTP подменяется RoundTripper-ом —
// скрейпер ходит на реальные URL basket-NN.wbbasket.ru, транспорт матчит по пути.

// ВАЖНО про потокобезопасность дублей ниже: tryBasket тянет price-history.json
// и card.json ДВУМЯ ГОРУТИНАМИ параллельно (wildberries.go), поэтому и фейковый
// транспорт, и фейковый кэш вызываются конкурентно. Без мьютексов `go test -race`
// падал на этих тестах (карта counts писалась из обеих горутин) — то есть
// `make test` был красным, хотя прод-код в порядке. Мьютекс только в дублях.
type fakeCondCache struct {
	mu sync.Mutex
	m  map[int64]CondEntry
}

func (f *fakeCondCache) GetCond(_ context.Context, id int64) (CondEntry, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	e, ok := f.m[id]
	return e, ok
}

func (f *fakeCondCache) PutCond(_ context.Context, id int64, e CondEntry) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.m[id] = e
}

// condCounts — счётчик запросов по файлам, безопасный для конкурентного
// транспорта.
type condCounts struct {
	mu sync.Mutex
	m  map[string]int
}

func newCondCounts() *condCounts { return &condCounts{m: map[string]int{}} }

func (c *condCounts) inc(k string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[k]++
}

func (c *condCounts) get(k string) int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.m[k]
}

// roundTripFunc уже объявлен в aliexpress_search_browser_test.go — переиспользуем.

func resp(status int, body string, hdr http.Header) *http.Response {
	if hdr == nil {
		hdr = http.Header{}
	}
	return &http.Response{
		StatusCode: status,
		Header:     hdr,
		Body:       io.NopCloser(strings.NewReader(body)),
	}
}

// condTestScraper — скрейпер с фейковым транспортом и in-memory cond-кэшем.
// Счётчики запросов по файлам возвращаются для ассертов.
func condTestScraper(t *testing.T, priceHandler func(*http.Request) *http.Response) (*WildberriesScraper, *fakeCondCache, *condCounts) {
	t.Helper()
	counts := newCondCounts()
	cache := &fakeCondCache{m: map[int64]CondEntry{}}
	s := NewWildberriesScraper(1000)
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/price-history.json"):
			counts.inc("price")
			return priceHandler(r), nil
		case strings.HasSuffix(r.URL.Path, "/card.json"):
			counts.inc("card")
			return resp(200, `{"imt_name":"Тестовый товар"}`, nil), nil
		default:
			return resp(404, "", nil), nil
		}
	})}
	s.SetCondCache(cache)
	return s, cache, counts
}

const condTestURL = "https://www.wildberries.ru/catalog/221501024/detail.aspx"

// condTestArticle — тот же артикул для тестов слоя архива напрямую: цену Scrape
// берёт только с живой карточки, а conditional GET живёт в fetchFromBasket.
const condTestArticle = "221501024"

func wbHistoryBody(priceKopecks int64) string {
	// Одна прошлая точка (для бэкфилла) + текущая (последняя = текущая цена).
	old := time.Now().Add(-10 * 24 * time.Hour).Unix()
	cur := time.Now().Add(-time.Hour).Unix()
	return fmt.Sprintf(`[{"dt":%d,"price":{"RUB":99900}},{"dt":%d,"price":{"RUB":%d}}]`, old, cur, priceKopecks)
}

func TestWBCondGetNotModified(t *testing.T) {
	full := 0
	s, cache, counts := condTestScraper(t, func(r *http.Request) *http.Response {
		if r.Header.Get("If-None-Match") == `"v1"` {
			return resp(http.StatusNotModified, "", nil)
		}
		full++
		h := http.Header{}
		h.Set("Etag", `"v1"`)
		h.Set("Last-Modified", "Mon, 29 Jun 2026 07:59:47 GMT")
		return resp(200, wbHistoryBody(123400), h)
	})

	// 1-й скрейп: снимка нет → полный путь (price-history + card), снимок записан.
	r1, err := s.fetchFromBasket(context.Background(), condTestArticle)
	if err != nil {
		t.Fatalf("первый скрейп: %v", err)
	}
	if r1.Price != 1234 || r1.Name != "Тестовый товар" || len(r1.History) != 1 {
		t.Fatalf("первый скрейп: price=%v name=%q history=%d", r1.Price, r1.Name, len(r1.History))
	}
	e, ok := cache.m[221501024]
	if !ok || e.ETag != `"v1"` || e.Price != 1234 || e.Name != "Тестовый товар" {
		t.Fatalf("снимок после полного скрейпа: %+v ok=%v", e, ok)
	}

	// 2-й скрейп: валидатор совпал → 304, Result из снимка, card.json не трогаем.
	r2, err := s.fetchFromBasket(context.Background(), condTestArticle)
	if err != nil {
		t.Fatalf("второй скрейп: %v", err)
	}
	if r2.Price != 1234 || r2.Name != "Тестовый товар" || !r2.InStock {
		t.Fatalf("Result из снимка: %+v", r2)
	}
	if len(r2.History) != 0 {
		t.Fatalf("на 304 History должен быть пуст (бэкфилл был на полном скрейпе), got %d", len(r2.History))
	}
	if full != 1 {
		t.Fatalf("полных ответов price-history: %d, want 1", full)
	}
	if counts.get("card") != 1 {
		t.Fatalf("card.json запрошен %d раз, want 1 (на 304 не запрашивается)", counts.get("card"))
	}
	if counts.get("price") != 2 {
		t.Fatalf("price-history запрошен %d раз, want 2", counts.get("price"))
	}
}

func TestWBCondGetModified(t *testing.T) {
	version := `"v1"`
	price := int64(123400)
	s, cache, counts := condTestScraper(t, func(r *http.Request) *http.Response {
		if r.Header.Get("If-None-Match") == version {
			return resp(http.StatusNotModified, "", nil)
		}
		h := http.Header{}
		h.Set("Etag", version)
		return resp(200, wbHistoryBody(price), h)
	})

	if _, err := s.fetchFromBasket(context.Background(), condTestArticle); err != nil {
		t.Fatalf("первый скрейп: %v", err)
	}

	// Цена сменилась → файл перегенерирован, новый ETag → conditional GET получает
	// 200, снимок обновляется, card.json дочитывается.
	version = `"v2"`
	price = 99000
	r, err := s.fetchFromBasket(context.Background(), condTestArticle)
	if err != nil {
		t.Fatalf("скрейп после смены цены: %v", err)
	}
	if r.Price != 990 {
		t.Fatalf("новая цена: %v, want 990", r.Price)
	}
	if e := cache.m[221501024]; e.ETag != `"v2"` || e.Price != 990 {
		t.Fatalf("снимок не обновился: %+v", e)
	}
	if counts.get("card") != 2 {
		t.Fatalf("card.json запрошен %d раз, want 2 (на 200 дочитывается)", counts.get("card"))
	}

	// Третий скрейп: новый валидатор совпал → снова 304.
	r3, err := s.fetchFromBasket(context.Background(), condTestArticle)
	if err != nil {
		t.Fatalf("третий скрейп: %v", err)
	}
	if r3.Price != 990 || counts.get("card") != 2 {
		t.Fatalf("после обновления снимка: price=%v card=%d", r3.Price, counts.get("card"))
	}
}

func TestWBCondGetNoValidators(t *testing.T) {
	// CDN без валидаторов (гипотетически): снимок не пишем, каждый скрейп полный.
	s, cache, counts := condTestScraper(t, func(*http.Request) *http.Response {
		return resp(200, wbHistoryBody(123400), nil)
	})

	for i := 0; i < 2; i++ {
		if _, err := s.fetchFromBasket(context.Background(), condTestArticle); err != nil {
			t.Fatalf("скрейп %d: %v", i+1, err)
		}
	}
	if len(cache.m) != 0 {
		t.Fatalf("снимок без валидаторов не должен писаться: %+v", cache.m)
	}
	if counts.get("card") != 2 {
		t.Fatalf("без снимка каждый скрейп полный: card=%d, want 2", counts.get("card"))
	}
}
