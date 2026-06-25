package main

import (
	"html/template"
	"log/slog"
	"strings"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

func TestRenderTemplate(t *testing.T) {
	h, err := NewWebHandlers(nil, nil, "https://tryberry.ru", "testver", slog.Default())
	if err != nil {
		t.Fatalf("NewWebHandlers: %v", err)
	}
	pd := pageData{
		PublicID:         "abcdef012345",
		Name:             "Тест <товар> & «кавычки»",
		MarketplaceLabel: "Ozon",
		Marketplace:      "ozon",
		ProductURL:       "https://ozon.ru/product/x",
		Canonical:        "https://tryberry.ru/p/abcdef012345/test",
		Title:            "Тест — история цены",
		Description:      "desc",
		Current:          1299,
		HasPrice:         true,
		InStock:          true,
		HasData:          true,
		Min30:            1100, Median30: 1399, Min90: 1050, MinAll: 990,
		SinceISO:   "2026-03-01",
		LDJSON:     template.JS(`{"@type":"Product"}`),
		SeriesJSON: template.JS(`{"points":[]}`),
	}
	html, err := h.render(pd)
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	s := string(html)
	for _, want := range []string{"abcdef012345", "299", "₽", "canonical", "application/ld+json", "/vendor/uPlot.iife.min.js"} {
		if !strings.Contains(s, want) {
			t.Errorf("rendered HTML missing %q", want)
		}
	}
	// XSS-страховка: имя экранируется (нет сырого <товар>).
	if strings.Contains(s, "<товар>") {
		t.Error("product name not HTML-escaped")
	}
}

func TestBuildSeries_ExtendsToNowAndMinMax(t *testing.T) {
	now := time.Date(2026, 6, 25, 12, 0, 0, 0, time.UTC)
	from := now.AddDate(0, 0, -90)
	// change-only: якорь (на from) + две смены; последняя дотягивается до now.
	pts := []postgres.PricePoint{
		{RecordedAt: from, Price: 1000},
		{RecordedAt: now.AddDate(0, 0, -10), Price: 800},
		{RecordedAt: now.AddDate(0, 0, -2), Price: 1200},
	}
	s := buildSeries(pts, 1200, now, "90d", from)

	if len(s.Points) != 4 {
		t.Fatalf("want 4 points (3 + now-extension), got %d", len(s.Points))
	}
	last := s.Points[len(s.Points)-1]
	if int64(last[0]) != now.UnixMilli() {
		t.Errorf("last point ts = %d, want now %d", int64(last[0]), now.UnixMilli())
	}
	if last[1] != 1200 {
		t.Errorf("last point price = %v, want 1200 (held to now)", last[1])
	}
	if s.Min != 800 || s.Max != 1200 {
		t.Errorf("min/max = %v/%v, want 800/1200", s.Min, s.Max)
	}
}

func TestBuildSeries_Empty(t *testing.T) {
	now := time.Now()
	s := buildSeries(nil, 0, now, "90d", now.AddDate(0, 0, -90))
	if len(s.Points) != 0 {
		t.Fatalf("empty input must yield 0 points, got %d", len(s.Points))
	}
}

func TestNormalizeRange(t *testing.T) {
	cases := map[string]string{
		"30d": "30d", "90d": "90d", "365d": "365d", "all": "all",
		"": defaultRange, "garbage": defaultRange, "7d": defaultRange,
	}
	for in, want := range cases {
		if got := normalizeRange(in); got != want {
			t.Errorf("normalizeRange(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRangeStart_AllUsesCreatedAt(t *testing.T) {
	now := time.Date(2026, 6, 25, 0, 0, 0, 0, time.UTC)
	created := now.AddDate(0, -8, 0)
	got := rangeStart("all", created, now)
	if got != created.Add(-time.Hour) {
		t.Errorf("rangeStart(all) = %v, want created-1h %v", got, created.Add(-time.Hour))
	}
}

func TestSlugify(t *testing.T) {
	cases := map[string]string{
		"Наушники Sony WH-1000XM5": "naushniki-sony-wh-1000xm5",
		"  Чайник!! 2.5 л  ":       "chaynik-2-5-l",
		"":                         "",
		"@#$%":                     "",
	}
	for in, want := range cases {
		if got := slugify(in); got != want {
			t.Errorf("slugify(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestRubFmt(t *testing.T) {
	const nb = " " // узкий неразрывный пробел (типографика разрядов и ₽)
	cases := map[float64]string{
		0:       "—",
		1234:    "1" + nb + "234" + nb + "₽",
		999:     "999" + nb + "₽",
		1234567: "1" + nb + "234" + nb + "567" + nb + "₽",
	}
	for in, want := range cases {
		if got := rubFmt(in); got != want {
			t.Errorf("rubFmt(%v) = %q, want %q", in, got, want)
		}
	}
}
