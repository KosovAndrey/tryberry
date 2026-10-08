package scraper

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Тесты живой цены WB: цена берётся с u-card (real-time, через xray), а
// basket-CDN price-history.json остаётся только источником Истории/имени/картинки.
// Суть фикса 2026-07-16: последняя точка архива — НЕ текущая цена, архив отстаёт
// на дни.
//
// Транспорт подменён: s.http обслуживает basket-CDN, s.ucard — u-card.

// ucardScraper — скрейпер, у которого архив отдаёт archiveKopecks (0 → товара в
// basket нет), а u-card отвечает тем, что вернёт ucardHandler. Счётчик обращений
// к u-card — для ассертов «сходили/не сходили».
func ucardScraper(t *testing.T, archiveKopecks int64, ucardHandler func() *http.Response) (*WildberriesScraper, *int) {
	t.Helper()
	s := NewWildberriesScraper(1000)
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/price-history.json"):
			if archiveKopecks == 0 {
				return resp(404, "", nil), nil // нет в basket (трансгран/удалён)
			}
			return resp(200, wbHistoryBody(archiveKopecks), nil), nil
		case strings.HasSuffix(r.URL.Path, "/card.json"):
			return resp(200, `{"imt_name":"Тестовый товар"}`, nil), nil
		default:
			return resp(404, "", nil), nil
		}
	})}
	calls := 0
	s.ucard = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Host != "card.wb.ru" || r.URL.Query().Get("nm") == "" {
			t.Errorf("u-card: неожиданный запрос %s", r.URL)
		}
		return ucardHandler(), nil
	})}
	return s, &calls
}

// wbUCardBody — ответ u-card (форма совпадает с поисковой выдачей). Живой товар
// идёт с остатками: наличие определяется по складам, а не по «цена > 0» — WB
// держит цену в карточке и после того, как товар кончился (инцидент 01-09-2026).
func wbUCardBody(priceKopecks int64) string {
	if priceKopecks == 0 {
		return `{"products":[{"id":221501024,"name":"Живой товар","totalQuantity":0,"sizes":[{"stocks":[]}]}]}`
	}
	return fmt.Sprintf(
		`{"products":[{"id":221501024,"name":"Живой товар","totalQuantity":7,`+
			`"sizes":[{"price":{"product":%d,"total":%d,"basic":%d},"stocks":[{"wh":1,"qty":7}]}]}]}`,
		priceKopecks, priceKopecks, priceKopecks)
}

// wbUCardBodyNoStock — цена ЕСТЬ, остатков нет. Ровно тот случай, что увёл
// пропавший товар в дайджест «лучших цен» 01-09-2026.
func wbUCardBodyNoStock(priceKopecks int64) string {
	return fmt.Sprintf(
		`{"products":[{"id":221501024,"name":"Живой товар","totalQuantity":0,`+
			`"sizes":[{"price":{"product":%d,"total":%d,"basic":%d},"stocks":[]}]}]}`,
		priceKopecks, priceKopecks, priceKopecks)
}

// Главный сценарий бага: архив говорит 3696 (точка от 13.07), u-card — живые 5047.
// Побеждает живая цена, а История для бэкфилла всё равно приезжает из архива.
func TestWBUCardBeatsArchive(t *testing.T) {
	s, calls := ucardScraper(t, 369602, func() *http.Response {
		return resp(200, wbUCardBody(504700), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.Price != 5047 {
		t.Errorf("цена = %v, ждали живую 5047 (архивная 3696.02 = тот самый баг)", r.Price)
	}
	if !r.InStock {
		t.Error("InStock = false при живой цене > 0")
	}
	if len(r.History) == 0 {
		t.Error("История пуста — архив должен остаться источником бэкфилла")
	}
	if r.Name != "Тестовый товар" {
		t.Errorf("имя = %q, ждали архивное (card.json полнее)", r.Name)
	}
	if *calls != 1 {
		t.Errorf("обращений к u-card = %d, ждали 1", *calls)
	}
}

// u-card не ответил (лёг xray / 403) → ErrLivePriceUnavailable, архивную цену НЕ
// отдаём: она отстаёт на дни, и notifier срабатывал по ней (решение 2026-10-08).
func TestWBUCardDownNoArchivePrice(t *testing.T) {
	s, _ := ucardScraper(t, 369602, func() *http.Response {
		return resp(http.StatusForbidden, "", nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if !errors.Is(err, ErrLivePriceUnavailable) {
		t.Fatalf("err = %v, ждали ErrLivePriceUnavailable", err)
	}
	if r != nil {
		t.Errorf("результат = %+v, ждали nil (архивная цена 3696.02 = тот самый баг)", r)
	}
}

// Живая карточка ответила пустой выдачей → «не нашли», а не сбой источника.
func TestWBUCardEmptyIsNotFound(t *testing.T) {
	s, _ := ucardScraper(t, 369602, func() *http.Response {
		return resp(200, `{"products":[]}`, nil)
	})
	if _, err := s.Scrape(context.Background(), condTestURL); !errors.Is(err, ErrProductNotFound) {
		t.Fatalf("err = %v, ждали ErrProductNotFound", err)
	}
}

// Живая карточка без активного оффера → честный OOS. Архив наличия не знает в
// принципе и всегда давал InStock=true.
func TestWBUCardReportsOutOfStock(t *testing.T) {
	s, _ := ucardScraper(t, 369602, func() *http.Response {
		return resp(200, wbUCardBody(0), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.InStock {
		t.Error("InStock = true, хотя живого оффера нет")
	}
	if r.Price != 0 {
		t.Errorf("цена = %v, ждали 0 при отсутствии оффера", r.Price)
	}
}

// Товара нет в basket (трансгран/удалён), но u-card его знает → отдаём живую
// карточку без Истории, а не ErrProductNotFound.
func TestWBUCardWorksWithoutArchive(t *testing.T) {
	s, _ := ucardScraper(t, 0, func() *http.Response {
		return resp(200, wbUCardBody(504700), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.Price != 5047 || r.Name != "Живой товар" {
		t.Errorf("price=%v name=%q, ждали живую карточку", r.Price, r.Name)
	}
}

// Цена в карточке есть, а остатков нет: WB показывает такой товар как «нет в
// наличии». Раньше мы считали его живым и советовали купить (инцидент 01-09-2026).
func TestWBUCardPriceWithoutStockIsOOS(t *testing.T) {
	s, _ := ucardScraper(t, 369602, func() *http.Response {
		return resp(200, wbUCardBodyNoStock(504700), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.InStock {
		t.Error("InStock = true при пустых остатках — это и есть баг дайджеста")
	}
	if r.StockUnknown {
		t.Error("StockUnknown = true, хотя u-card наличие видит")
	}
}
