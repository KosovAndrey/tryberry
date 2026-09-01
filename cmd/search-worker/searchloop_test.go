package main

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

func TestShouldEvaluate(t *testing.T) {
	now := time.Date(2026, 6, 9, 12, 0, 0, 0, time.UTC)
	ago := func(d time.Duration) *time.Time { v := now.Add(-d); return &v }

	const (
		reseller = time.Minute
		normal   = 20 * time.Minute
	)

	cases := []struct {
		name     string
		lastEval *time.Time
		interval time.Duration
		want     bool
	}{
		{"никогда не оценивалась", nil, normal, true},
		{"перекуп: прошла минута → оцениваем", ago(60 * time.Second), reseller, true},
		{"перекуп: чуть раньше (в пределах slack) → оцениваем", ago(58 * time.Second), reseller, true},
		{"перекуп: 10с назад → ждём", ago(10 * time.Second), reseller, false},
		// Обычный подписчик на перекуп-запросе: выдача скрейпится раз в минуту,
		// но оценка не чаще ~20 мин — фаст-частота к нему не протекает.
		{"обычный: 1 минута назад → ждём (обузили)", ago(time.Minute), normal, false},
		{"обычный: 5 минут назад → ждём", ago(5 * time.Minute), normal, false},
		{"обычный: 20 минут назад → оцениваем", ago(20 * time.Minute), normal, true},
		{"обычный: 19м58с (в пределах slack) → оцениваем", ago(19*time.Minute + 58*time.Second), normal, true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := shouldEvaluate(c.lastEval, c.interval, now); got != c.want {
				t.Fatalf("shouldEvaluate=%v, want %v", got, c.want)
			}
		})
	}
}

// ── scrapeQuery: маркетплейс скрейпера + канон URL ─────────────────────────────
//
// Регресс-щит на баг, что дважды уезжал в scrapeQuery незаметно для сборки и
// метрик (2026-07-16): (1) маркетплейс был константой "wildberries" → 3119
// чужих товаров с ярлыком WB; (2) правку на q.Marketplace — а q собран из
// Kafka-задачи, где поля Marketplace НЕТ (SearchTask его не несёт) → пустая
// метка + канон URL молча не срабатывал. Правильный источник — ss.Marketplace().
//
// Тест гоняет scrapeQuery с фейк-реестром (без БД/Kafka) и проверяет, что каждый
// апсертнутый товар получает: (а) маркетплейс СКРЕЙПЕРА выдачи, а не WB/пустой;
// (б) канонический URL (CanonicalProductURL), а не сырую ссылку из выдачи со
// слагом/utm. Обе гарантии — на одном прогоне, потому что оба бага их ломали.

type fakeSearchScraper struct {
	mp  scraper.Marketplace
	set *scraper.SearchResultSet
}

func (f *fakeSearchScraper) Marketplace() scraper.Marketplace { return f.mp }
func (f *fakeSearchScraper) Matches(string) bool              { return true }
func (f *fakeSearchScraper) Scrape(context.Context, string) (*scraper.Result, error) {
	return nil, nil
}
func (f *fakeSearchScraper) MatchesSearch(string) bool                   { return true }
func (f *fakeSearchScraper) NormalizeSearchURL(u string) (string, error) { return u, nil }
func (f *fakeSearchScraper) ScrapeSearch(context.Context, string) (*scraper.SearchResultSet, error) {
	return f.set, nil
}

type fakeRegistry struct{ ss scraper.SearchScraper }

func (r fakeRegistry) FindSearchByURL(string) (scraper.SearchScraper, error) { return r.ss, nil }

type fakeProductStore struct{ got []postgres.ProductUpsert }

func (s *fakeProductStore) UpsertBatch(_ context.Context, items []postgres.ProductUpsert) (map[string]int64, error) {
	s.got = append(s.got, items...)
	out := make(map[string]int64, len(items))
	for i, it := range items {
		out[it.URL] = int64(i + 1) // id по URL — ключ должен быть КАНОНОМ, как в scrapeQuery
	}
	return out, nil
}

type fakeQueryStore struct{}

func (fakeQueryStore) UpdateLastScraped(context.Context, int64) error       { return nil }
func (fakeQueryStore) UpdateMinPrice(context.Context, int64, float64) error { return nil }

type fakeResultStore struct{ rows []postgres.ResultUpsert }

func (s *fakeResultStore) UpsertBatch(_ context.Context, _ int64, rows []postgres.ResultUpsert) error {
	s.rows = append(s.rows, rows...)
	return nil
}

// fakeSubStore без активных подписок → evaluateSubscription не вызывается,
// поэтому notifs/events в этом тесте не нужны (оставлены nil).
type fakeSubStore struct{}

func (fakeSubStore) GetActiveByQueryID(context.Context, int64) ([]*domain.SearchSubscription, error) {
	return nil, nil
}
func (fakeSubStore) GetBaseline(context.Context, int64, int64) (float64, bool, error) {
	return 0, false, nil
}
func (fakeSubStore) UpsertBaseline(context.Context, int64, int64, float64) error { return nil }
func (fakeSubStore) MarkEvaluated(context.Context, int64) error                  { return nil }

func TestScrapeQueryMarketplaceAndCanon(t *testing.T) {
	cases := []struct {
		name      string
		mp        scraper.Marketplace
		itemURL   string
		wantMP    string
		wantCanon string
	}{
		{
			name:      "ozon: слаг+utm → канон /product/<id>/, метка ozon",
			mp:        scraper.MarketplaceOzon,
			itemURL:   "https://www.ozon.ru/product/nabor-posudy-987654321/?asb=abc",
			wantMP:    "ozon",
			wantCanon: "https://www.ozon.ru/product/987654321/",
		},
		{
			// Живая форма карточки YM с 01-09-2026 — /card/<slug>/<oskuId>; канон
			// сворачивает слаг в заглушку и срезает query.
			name:      "yandex_market: /card/<slug>/<id>?utm → канон /card/x/<id>",
			mp:        scraper.MarketplaceYandexMarket,
			itemURL:   "https://market.yandex.ru/card/smartfon/5193397317?utm_source=x",
			wantMP:    "yandex_market",
			wantCanon: "https://market.yandex.ru/card/x/5193397317",
		},
		{
			// Мёртвая /product-форма канон не трогает: resolve modelId → oskuId
			// живёт только в выдаче. docs/YANDEX-CARD-MIGRATION.md.
			name:      "yandex_market: мёртвая /product--форма остаётся как есть",
			mp:        scraper.MarketplaceYandexMarket,
			itemURL:   "https://market.yandex.ru/product--smartfon/5193397317?utm_source=x",
			wantMP:    "yandex_market",
			wantCanon: "https://market.yandex.ru/product--smartfon/5193397317?utm_source=x",
		},
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			prod := &fakeProductStore{}
			res := &fakeResultStore{}
			w := &searchWorker{
				log: slog.New(slog.NewTextHandler(io.Discard, nil)),
				registry: fakeRegistry{ss: &fakeSearchScraper{
					mp: c.mp,
					set: &scraper.SearchResultSet{Items: []scraper.SearchItem{
						{URL: c.itemURL, Name: "тест", Position: 1, PriceKopecks: 100000},
					}},
				}},
				queries:  fakeQueryStore{},
				subs:     fakeSubStore{},
				results:  res,
				products: prod,
			}

			// URL задачи неважен — фейк-реестр всё равно вернёт наш скрейпер; важно,
			// что маркетплейс берётся из НЕГО, а не из задачи (у неё поля нет).
			q := &domain.SearchQuery{ID: 1, NormalizedURL: "https://irrelevant/search"}
			if err := w.scrapeQuery(context.Background(), q); err != nil {
				t.Fatalf("scrapeQuery: %v", err)
			}

			if len(prod.got) != 1 {
				t.Fatalf("upserts=%d, want 1", len(prod.got))
			}
			up := prod.got[0]
			if up.Marketplace != c.wantMP {
				t.Errorf("marketplace=%q, want %q (должен быть у СКРЕЙПЕРА, не WB/пустой)", up.Marketplace, c.wantMP)
			}
			if up.URL != c.wantCanon {
				t.Errorf("url=%q, want канон %q", up.URL, c.wantCanon)
			}
			// Результат выдачи должен ссылаться на апсертнутый товар — значит канон,
			// которым писали products, и канон, которым читали id, совпали.
			if len(res.rows) != 1 {
				t.Fatalf("result rows=%d, want 1 (канон products и lookup разъехались?)", len(res.rows))
			}
		})
	}
}
