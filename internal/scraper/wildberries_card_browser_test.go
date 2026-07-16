package scraper

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

// Тесты живой цены WB через браузер-сайдкар (/card). Суть фикса 2026-07-16: цена
// приходит с ЖИВОЙ карточки, а basket-CDN price-history.json (отстаёт на дни)
// остаётся только источником Истории/имени/картинки.
//
// Транспорт подменён: s.http обслуживает basket-CDN, s.cardBrowserClient — сайдкар.

// cardBrowserScraper — скрейпер, у которого архив отдаёт archiveKopecks, а
// сайдкар отвечает тем, что вернёт cardHandler. Счётчик обращений к сайдкару — для
// ассертов «сходили/не сходили».
func cardBrowserScraper(t *testing.T, archiveKopecks int64, cardHandler func() *http.Response) (*WildberriesScraper, *int) {
	t.Helper()
	s := NewWildberriesScraper(1000)
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/price-history.json"):
			if archiveKopecks == 0 {
				return resp(404, "", nil), nil // товара нет в basket (трансгран/удалён)
			}
			return resp(200, wbHistoryBody(archiveKopecks), nil), nil
		case strings.HasSuffix(r.URL.Path, "/card.json"):
			return resp(200, `{"imt_name":"Тестовый товар"}`, nil), nil
		default:
			return resp(404, "", nil), nil
		}
	})}
	// u-card-фолбэк (архив 404 → трансгран/удалён) тоже глушим: иначе тест уходит
	// в живой u-card.wb.ru и проверяет интернет, а не нас.
	s.ucard = &http.Client{Transport: roundTripFunc(func(*http.Request) (*http.Response, error) {
		return resp(404, "", nil), nil
	})}
	calls := 0
	s.SetCardBrowser("http://wb-search-miner:8081")
	s.cardBrowserClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if !strings.HasSuffix(r.URL.Path, "/card") || r.URL.Query().Get("nm") == "" {
			t.Errorf("сайдкар: неожиданный запрос %s", r.URL)
		}
		return cardHandler(), nil
	})}
	return s, &calls
}

// wbCardBody — ответ u-card (форма = поисковой выдаче).
func wbCardBody(priceKopecks int64) string {
	return fmt.Sprintf(
		`{"products":[{"id":221501024,"name":"Живой товар","sizes":[{"price":{"product":%d,"total":%d,"basic":%d}}]}]}`,
		priceKopecks, priceKopecks, priceKopecks)
}

// Главный сценарий бага: архив говорит 3696, живая карточка — 5047. Побеждает живая,
// а История (бэкфилл) при этом всё равно приезжает из архива.
func TestWBCardBrowserBeatsArchive(t *testing.T) {
	s, calls := cardBrowserScraper(t, 369602, func() *http.Response {
		return resp(200, wbCardBody(504700), nil)
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
	if *calls != 1 {
		t.Errorf("обращений к сайдкару = %d, ждали 1", *calls)
	}
	// Имя/картинка — из архива (card.json), у него они полнее.
	if r.Name != "Тестовый товар" {
		t.Errorf("имя = %q, ждали архивное", r.Name)
	}
}

// Сайдкар лежит (нет прогретых дорожек) → не падаем, отдаём архивную цену: она
// отстаёт, но это лучше, чем ничего. Долю видно по wb_price_source{source=basket}.
func TestWBCardBrowserDownFallsBackToArchive(t *testing.T) {
	s, _ := cardBrowserScraper(t, 369602, func() *http.Response {
		return resp(http.StatusBadGateway, "no healthy lanes", nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.Price != 3696.02 {
		t.Errorf("цена = %v, ждали архивную 3696.02", r.Price)
	}
}

// Живая карточка без активного оффера → честный OOS. Раньше InStock был
// захардкожен в true, и «нет в продаже» у WB не отличался от «продаётся».
func TestWBCardBrowserReportsOutOfStock(t *testing.T) {
	s, _ := cardBrowserScraper(t, 369602, func() *http.Response {
		return resp(200, wbCardBody(0), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.InStock {
		t.Error("InStock = true, хотя живой оффер отсутствует")
	}
	if r.Price != 0 {
		t.Errorf("цена = %v, ждали 0 при отсутствии оффера", r.Price)
	}
}

// Пустой products — это сбой формы ответа, а НЕ «товар не продаётся». Иначе один
// такой сбой разом погасил бы все товары WB. Ждём откат в архив, не OOS.
func TestWBCardBrowserEmptyProductsIsNotOOS(t *testing.T) {
	s, _ := cardBrowserScraper(t, 369602, func() *http.Response {
		return resp(200, `{"products":[]}`, nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if !r.InStock || r.Price != 3696.02 {
		t.Errorf("price=%v instock=%v, ждали откат в архив (3696.02/true), а не OOS", r.Price, r.InStock)
	}
}

// Товара нет в basket (трансгран/удалён), но живая карточка есть → отдаём её
// без Истории, а не ErrProductNotFound.
func TestWBCardBrowserWorksWithoutArchive(t *testing.T) {
	s, _ := cardBrowserScraper(t, 0, func() *http.Response {
		return resp(200, wbCardBody(504700), nil)
	})
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.Price != 5047 || r.Name != "Живой товар" {
		t.Errorf("price=%v name=%q, ждали живую карточку", r.Price, r.Name)
	}
}

// Рубильник отката: сайдкар не настроен → поведение ровно как до фикса.
func TestWBCardBrowserDisabledKeepsArchivePath(t *testing.T) {
	s, calls := cardBrowserScraper(t, 369602, func() *http.Response {
		return resp(200, wbCardBody(504700), nil)
	})
	s.SetCardBrowser("")
	r, err := s.Scrape(context.Background(), condTestURL)
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	if r.Price != 3696.02 {
		t.Errorf("цена = %v, ждали архивную 3696.02", r.Price)
	}
	if *calls != 0 {
		t.Errorf("обращений к сайдкару = %d, ждали 0 при пустом WB_CARD_BROWSER_URL", *calls)
	}
}
