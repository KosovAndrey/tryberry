package main

import (
	"context"
	"embed"
	"encoding/json"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// webpages.go — ПУБЛИЧНЫЕ read-only веб-страницы графиков цены (/p/<public_id>) и
// их JSON-API. Живут в сервисе api: у него уже есть пул Postgres и http-сервер.
// Никаких user/subscription данных тут не отдаётся — только товар и его цена.

//go:embed templates/product.html
var productTemplateFS embed.FS

// publicIDRe — строгая форма токена из миграции 022 (12 hex). Всё иное → 404,
// без похода в БД.
var publicIDRe = regexp.MustCompile(`^[0-9a-f]{12}$`)

const (
	webReqTimeout   = 3 * time.Second
	webCacheTTL     = 120 * time.Second // in-proc кэш страниц/серий
	pageCacheMaxAge = 300               // Cache-Control max-age, сек
	sitemapMaxURLs  = 5000
	defaultRange    = "90d"
)

// marketplaceLabels — человекочитаемые названия для бейджа и SEO-текста.
var marketplaceLabels = map[string]string{
	"ozon":          "Ozon",
	"wildberries":   "Wildberries",
	"yandex_market": "Яндекс Маркет",
	"aliexpress":    "AliExpress",
}

type productRepo interface {
	GetByPublicID(ctx context.Context, publicID string) (*domain.Product, bool, error)
	ListPublicForSitemap(ctx context.Context, limit int) ([]postgres.SitemapEntry, error)
}

type priceRepo interface {
	Series(ctx context.Context, productID int64, from, to time.Time) ([]postgres.PricePoint, error)
	Stats(ctx context.Context, productID int64, now time.Time) (domain.PriceStats, error)
	GetLatest(ctx context.Context, productID int64) (float64, time.Time, error)
}

// WebHandlers — обработчики публичного веб-раздела.
type WebHandlers struct {
	products productRepo
	prices   priceRepo
	baseURL  string
	log      *slog.Logger
	tmpl     *template.Template

	cache *ttlCache
}

// NewWebHandlers парсит шаблон и собирает обработчики. baseURL — без хвостового
// слэша (например https://tryberry.ru), идёт в canonical/OG/sitemap.
func NewWebHandlers(products productRepo, prices priceRepo, baseURL string, log *slog.Logger) (*WebHandlers, error) {
	tmpl, err := template.New("product.html").Funcs(templateFuncs).ParseFS(productTemplateFS, "templates/product.html")
	if err != nil {
		return nil, fmt.Errorf("parse product template: %w", err)
	}
	return &WebHandlers{
		products: products,
		prices:   prices,
		baseURL:  strings.TrimRight(baseURL, "/"),
		log:      log,
		tmpl:     tmpl,
		cache:    newTTLCache(),
	}, nil
}

// Register монтирует публичные маршруты на mux.
func (h *WebHandlers) Register(mux *http.ServeMux) {
	mux.HandleFunc("/p/", h.handleProductPage)
	mux.HandleFunc("/api/price-history", h.handlePriceHistory)
	mux.HandleFunc("/sitemap.xml", h.handleSitemap)
	mux.HandleFunc("/robots.txt", h.handleRobots)
}

// ── /p/<public_id>[/slug] ───────────────────────────────────────────────────

func (h *WebHandlers) handleProductPage(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	rest := strings.TrimPrefix(r.URL.Path, "/p/")
	publicID := rest
	if i := strings.IndexByte(rest, '/'); i >= 0 {
		publicID = rest[:i]
	}
	if !publicIDRe.MatchString(publicID) {
		h.notFound(w)
		return
	}

	if html, ok := h.cache.getPage(publicID); ok {
		h.writePage(w, html)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webReqTimeout)
	defer cancel()

	p, inStock, err := h.products.GetByPublicID(ctx, publicID)
	if err != nil {
		if err == domain.ErrNotFound {
			h.notFound(w)
			return
		}
		h.log.Error("product page: load product", "err", err, "public_id", publicID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	current, _, _ := h.prices.GetLatest(ctx, p.ID)
	stats, _ := h.prices.Stats(ctx, p.ID, now)

	from := rangeStart(defaultRange, p.CreatedAt, now)
	points, err := h.prices.Series(ctx, p.ID, from, now)
	if err != nil {
		h.log.Error("product page: load series", "err", err, "product_id", p.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	// Каноничный URL со слагом: резолв всё равно по public_id, слаг декоративен.
	slug := slugify(p.Name)
	canonical := h.baseURL + "/p/" + publicID
	if slug != "" {
		canonical += "/" + slug
	}

	seriesJSON, _ := json.Marshal(buildSeries(points, current, now, defaultRange, from))

	pd := h.buildPageData(p, inStock, current, stats, canonical, now, template.JS(seriesJSON))

	html, err := h.render(pd)
	if err != nil {
		h.log.Error("product page: render", "err", err, "product_id", p.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	h.cache.setPage(publicID, html)
	h.writePage(w, html)
}

func (h *WebHandlers) writePage(w http.ResponseWriter, html []byte) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", pageCacheMaxAge))
	_, _ = w.Write(html)
}

func (h *WebHandlers) notFound(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Robots-Tag", "noindex")
	w.WriteHeader(http.StatusNotFound)
	_, _ = w.Write([]byte(`<!doctype html><meta charset="utf-8"><title>Не найдено</title>` +
		`<body style="font-family:sans-serif;background:#170711;color:#f9f3ef;text-align:center;padding:80px 20px">` +
		`<h1>Товар не найден</h1><p><a style="color:#ff5d8f" href="/">На главную</a></p>`))
}

// ── /api/price-history?p=<public_id>&range=30d|90d|365d|all ──────────────────

func (h *WebHandlers) handlePriceHistory(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	publicID := r.URL.Query().Get("p")
	if !publicIDRe.MatchString(publicID) {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	rng := normalizeRange(r.URL.Query().Get("range"))

	if body, ok := h.cache.getSeries(publicID, rng); ok {
		h.writeJSON(w, body)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), webReqTimeout)
	defer cancel()

	p, _, err := h.products.GetByPublicID(ctx, publicID)
	if err != nil {
		if err == domain.ErrNotFound {
			http.Error(w, "not found", http.StatusNotFound)
			return
		}
		h.log.Error("price-history: load product", "err", err, "public_id", publicID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	now := time.Now()
	current, _, _ := h.prices.GetLatest(ctx, p.ID)
	from := rangeStart(rng, p.CreatedAt, now)
	points, err := h.prices.Series(ctx, p.ID, from, now)
	if err != nil {
		h.log.Error("price-history: load series", "err", err, "product_id", p.ID)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	body, _ := json.Marshal(buildSeries(points, current, now, rng, from))
	h.cache.setSeries(publicID, rng, body)
	h.writeJSON(w, body)
}

func (h *WebHandlers) writeJSON(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", pageCacheMaxAge))
	_, _ = w.Write(body)
}

// ── /sitemap.xml ────────────────────────────────────────────────────────────

func (h *WebHandlers) handleSitemap(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if body, ok := h.cache.getSitemap(); ok {
		h.writeXML(w, body)
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	entries, err := h.products.ListPublicForSitemap(ctx, sitemapMaxURLs)
	if err != nil {
		h.log.Error("sitemap: list", "err", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}

	var b strings.Builder
	b.WriteString(`<?xml version="1.0" encoding="UTF-8"?>` + "\n")
	b.WriteString(`<urlset xmlns="http://www.sitemaps.org/schemas/sitemap/0.9">` + "\n")
	for _, e := range entries {
		loc := h.baseURL + "/p/" + e.PublicID
		if slug := slugify(e.Name); slug != "" {
			loc += "/" + slug
		}
		b.WriteString("  <url><loc>")
		b.WriteString(xmlEscape(loc))
		b.WriteString("</loc><lastmod>")
		b.WriteString(e.UpdatedAt.UTC().Format("2006-01-02"))
		b.WriteString("</lastmod><changefreq>daily</changefreq></url>\n")
	}
	b.WriteString("</urlset>\n")

	body := []byte(b.String())
	h.cache.setSitemap(body)
	h.writeXML(w, body)
}

func (h *WebHandlers) writeXML(w http.ResponseWriter, body []byte) {
	w.Header().Set("Content-Type", "application/xml; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=3600")
	_, _ = w.Write(body)
}

func (h *WebHandlers) handleRobots(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=86400")
	_, _ = fmt.Fprintf(w, "User-agent: *\nAllow: /\nSitemap: %s/sitemap.xml\n", h.baseURL)
}
