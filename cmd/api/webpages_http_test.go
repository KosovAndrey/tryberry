package main

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// ── стабы репозиториев (без БД) ──────────────────────────────────────────────

type stubProducts struct {
	prod *domain.Product
	in   bool
}

func (s *stubProducts) GetByPublicID(_ context.Context, publicID string) (*domain.Product, bool, error) {
	if s.prod == nil || s.prod.PublicID != publicID {
		return nil, false, domain.ErrNotFound
	}
	return s.prod, s.in, nil
}
func (s *stubProducts) ListPublicForSitemap(_ context.Context, _ int) ([]postgres.SitemapEntry, error) {
	if s.prod == nil {
		return nil, nil
	}
	return []postgres.SitemapEntry{{PublicID: s.prod.PublicID, Name: s.prod.Name, UpdatedAt: s.prod.UpdatedAt}}, nil
}

type stubPrices struct {
	pts []postgres.PricePoint
}

func (s *stubPrices) Series(_ context.Context, _ int64, _, _ time.Time) ([]postgres.PricePoint, error) {
	return s.pts, nil
}
func (s *stubPrices) Stats(_ context.Context, _ int64, now time.Time) (domain.PriceStats, error) {
	// Since достаточно давно → вердикт считается (не Insufficient). current(1299)
	// ≤ Median30(1399) и > Min* → VerdictTypical → класс v-typical.
	return domain.PriceStats{
		HasData: true, MinAll: 990, Median30: 1399, Min90: 1050, Min30: 1100,
		CountAll: 8, Since: now.AddDate(0, 0, -60),
	}, nil
}
func (s *stubPrices) GetLatest(_ context.Context, _ int64) (float64, time.Time, error) {
	return 1299, time.Now(), nil
}

func newTestHandlers(t *testing.T) *WebHandlers {
	t.Helper()
	now := time.Now()
	prod := &domain.Product{ID: 7, PublicID: "abcdef012345", URL: "https://ozon.ru/p/x",
		Name: "Наушники", ImageURL: "https://cdn.example/img.webp", Marketplace: "ozon",
		CreatedAt: now.AddDate(0, 0, -120), UpdatedAt: now}
	pts := []postgres.PricePoint{
		{RecordedAt: now.AddDate(0, 0, -90), Price: 1500},
		{RecordedAt: now.AddDate(0, 0, -10), Price: 1299},
	}
	h, err := NewWebHandlers(&stubProducts{prod: prod, in: true}, &stubPrices{pts: pts}, "https://tryberry.ru", "testver", slog.Default())
	if err != nil {
		t.Fatalf("NewWebHandlers: %v", err)
	}
	return h
}

func TestProductPage_OK(t *testing.T) {
	h := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux)

	req := httptest.NewRequest(http.MethodGet, "/p/abcdef012345/naushniki", nil)
	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/html") {
		t.Errorf("content-type = %q", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "max-age") {
		t.Errorf("missing Cache-Control: %q", cc)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Наушники") || !strings.Contains(body, `window.__CHART__`) {
		t.Error("page body missing product name or chart bootstrap")
	}
	// JSON-LD должен попасть в страницу НЕэкранированным (кавычки не &#34;),
	// иначе поисковики не распарсят микроразметку.
	if !strings.Contains(body, `"@type":"Product"`) {
		t.Errorf("JSON-LD mangled/escaped in page; body excerpt missing raw schema")
	}
	// Тезис страницы — вердикт честной цены: класс на body + заголовок.
	if !strings.Contains(body, `class="v-typical"`) || !strings.Contains(body, "Обычная цена") {
		t.Error("verdict hero not rendered (expected v-typical / «Обычная цена»)")
	}
	// Опорные значения для линий графика прокинуты в bootstrap (html/template в
	// JS-контексте обрамляет числа пробелами — проверяем по наличию значений).
	if !strings.Contains(body, "refs:{min:") || !strings.Contains(body, "990") || !strings.Contains(body, "1399") {
		t.Error("reference values (min/usual) not injected into chart bootstrap")
	}
	// Ассеты версионированы хешем (?v=) — сброс кэша css/js при деплое.
	if !strings.Contains(body, "/assets/chart.js?v=testver") ||
		!strings.Contains(body, "/assets/chart.css?v=testver") ||
		!strings.Contains(body, "/vendor/uPlot.iife.min.js?v=testver") {
		t.Error("assets not versioned with ?v=")
	}
	// Картинка товара рендерится (с хотлинк-защитой no-referrer).
	if !strings.Contains(body, `id="prodimg"`) ||
		!strings.Contains(body, "https://cdn.example/img.webp") ||
		!strings.Contains(body, `referrerpolicy="no-referrer"`) {
		t.Error("product image not rendered with expected attributes")
	}
}

func TestProductPage_NotFoundAndBadID(t *testing.T) {
	h := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux)

	for _, path := range []string{"/p/ffffffffffff", "/p/BADID", "/p/", "/p/zz"} {
		rr := httptest.NewRecorder()
		mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
		if rr.Code != http.StatusNotFound {
			t.Errorf("%s: status = %d, want 404", path, rr.Code)
		}
		if rr.Header().Get("X-Robots-Tag") != "noindex" {
			t.Errorf("%s: 404 should carry noindex", path)
		}
	}
}

func TestPriceHistoryJSON(t *testing.T) {
	h := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/price-history?p=abcdef012345&range=30d", nil))
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type = %q", ct)
	}
	var resp seriesResp
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("json: %v; body=%s", err, rr.Body.String())
	}
	// 2 точки + дотяжка до now = 3.
	if len(resp.Points) != 3 {
		t.Errorf("points = %d, want 3", len(resp.Points))
	}
	if resp.Range != "30d" {
		t.Errorf("range = %q, want 30d", resp.Range)
	}
	// Эндпоинт НЕ должен светить лишние поля (user/subscription).
	if strings.Contains(rr.Body.String(), "user") || strings.Contains(rr.Body.String(), "subscription") {
		t.Error("JSON leaks unexpected fields")
	}
}

func TestPriceHistory_BadAndMissing(t *testing.T) {
	h := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/price-history?p=NOPE", nil))
	if rr.Code != http.StatusBadRequest {
		t.Errorf("bad id: status = %d, want 400", rr.Code)
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/price-history?p=ffffffffffff", nil))
	if rr.Code != http.StatusNotFound {
		t.Errorf("missing: status = %d, want 404", rr.Code)
	}
}

func TestSitemapAndRobots(t *testing.T) {
	h := newTestHandlers(t)
	mux := http.NewServeMux()
	h.Register(mux)

	rr := httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/sitemap.xml", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "<urlset") {
		t.Errorf("sitemap: code=%d body=%s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), "https://tryberry.ru/p/abcdef012345") {
		t.Error("sitemap missing product URL")
	}

	rr = httptest.NewRecorder()
	mux.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/robots.txt", nil))
	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "Sitemap: https://tryberry.ru/sitemap.xml") {
		t.Errorf("robots: code=%d body=%s", rr.Code, rr.Body.String())
	}
}
