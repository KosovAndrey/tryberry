package scraper

import (
	"context"
	"net/http"
	"strings"
	"testing"
)

// Живая цена через браузерный сайдкар. 23-09-2026 WB закрыл 403-м ВСЕ публичные
// хосты карточки разом (card/u-card), и единственным источником живой цены стал
// wb-search-miner: он делает in-page fetch к __internal из прогретого браузера и
// отдаёт JSON той же формы. Тесты держат именно это: при 403 публичного хоста
// цена приходит из сайдкара, а не из архива (архив отстаёт на дни).

// cardSidecarScraper — скрейпер, у которого архив отдаёт archiveKopecks, публичный
// хост карточки всегда 403, а запросы к сайдкару обслуживает handler. Транспорт
// подменён, а не поднят httptest-сервером: к локальному порту в этой среде не
// достучаться (WSL mirrored), да и сеть тесту не нужна.
func cardSidecarScraper(t *testing.T, archiveKopecks int64, sidecar func(*http.Request) *http.Response) (*WildberriesScraper, *int) {
	t.Helper()
	s := NewWildberriesScraper(1000)
	s.SetUCardPrimary(true)
	s.SetCardBrowserSidecar("http://wb-search-miner:8081")
	s.http = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/price-history.json"):
			if archiveKopecks == 0 {
				return resp(404, "", nil), nil
			}
			return resp(200, wbHistoryBody(archiveKopecks), nil), nil
		case strings.HasSuffix(r.URL.Path, "/card.json"):
			return resp(200, `{"imt_name":"Тестовый товар"}`, nil), nil
		default:
			return resp(404, "", nil), nil
		}
	})}
	// Публичный хост закрыт — как в проде с 23-09-2026.
	s.ucard = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(403, "", nil), nil
	})}
	calls := 0
	s.cardBrowser = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		calls++
		if r.URL.Path != "/card" {
			t.Errorf("сайдкар: неожиданный путь %s", r.URL.Path)
		}
		return sidecar(r), nil
	})}
	return s, &calls
}

func TestCardBrowserFallbackServesLivePrice(t *testing.T) {
	var gotNM string
	s, calls := cardSidecarScraper(t, 500000, func(r *http.Request) *http.Response {
		gotNM = r.URL.Query().Get("nm")
		return resp(200, wbUCardBody(69900), nil)
	})

	res, err := s.Scrape(context.Background(), "https://www.wildberries.ru/catalog/211695539/detail.aspx")
	if err != nil {
		t.Fatalf("скрейп упал: %v", err)
	}
	if *calls != 1 {
		t.Fatalf("сайдкар звали %d раз, ждали 1", *calls)
	}
	if gotNM != "211695539" {
		t.Fatalf("сайдкару ушёл nm=%q", gotNM)
	}
	// 699 из браузера, а не 5000 из архива.
	if res.Price != 699 {
		t.Fatalf("цена %v — взята не из сайдкара", res.Price)
	}
}

func TestCardBrowserFallbackFallsBackToArchive(t *testing.T) {
	// Нет прогретых дорожек: сайдкар отвечает 502. Цена тогда из архива —
	// устаревшая, но бот остаётся живым (тот же размен, что у WB_UCARD_PRIMARY).
	s, calls := cardSidecarScraper(t, 500000, func(r *http.Request) *http.Response {
		return resp(502, "no healthy lanes", nil)
	})

	res, err := s.Scrape(context.Background(), "https://www.wildberries.ru/catalog/211695539/detail.aspx")
	if err != nil {
		t.Fatalf("скрейп упал: %v", err)
	}
	if *calls == 0 {
		t.Fatal("сайдкар не позвали")
	}
	if res.Price != 5000 {
		t.Fatalf("цена %v — ждали архивные 5000", res.Price)
	}
}

func TestCardBrowserNotConfiguredKeepsOldPath(t *testing.T) {
	// Пустой URL — фолбэка нет: ходим только публичным хостом, как раньше.
	s, calls := cardSidecarScraper(t, 500000, func(r *http.Request) *http.Response {
		return resp(200, wbUCardBody(69900), nil)
	})
	s.SetCardBrowserSidecar("")

	if _, err := s.Scrape(context.Background(), "https://www.wildberries.ru/catalog/211695539/detail.aspx"); err != nil {
		t.Fatalf("скрейп упал: %v", err)
	}
	if *calls != 0 {
		t.Fatalf("сайдкар звали %d раз при пустом URL", *calls)
	}
}

func TestCardBrowserRetriesAfterChallenge(t *testing.T) {
	// 498 = у дорожки протух токен wbaas; сайдкар метит её нездоровой и чинит
	// фоном, а вторая попытка попадает на соседнюю. Без ретрая товар молча
	// уезжал бы на архивную цену, отставшую на дни.
	n := 0
	s, calls := cardSidecarScraper(t, 500000, func(r *http.Request) *http.Response {
		n++
		if n == 1 {
			return resp(498, "", nil)
		}
		return resp(200, wbUCardBody(69900), nil)
	})

	res, err := s.Scrape(context.Background(), "https://www.wildberries.ru/catalog/211695539/detail.aspx")
	if err != nil {
		t.Fatalf("скрейп упал: %v", err)
	}
	if *calls != 2 {
		t.Fatalf("сайдкар звали %d раз, ждали 2", *calls)
	}
	if res.Price != 699 {
		t.Fatalf("цена %v — ретрай не спас живую цену", res.Price)
	}
}

func TestCardBrowserDownFallsBackToPublicHost(t *testing.T) {
	// Сайдкар лёг, а публичный хост жив (так будет, когда WB откроет ручки
	// обратно — он уже дважды менял их местами). Цена должна прийти оттуда, а
	// не с архива, отстающего на дни.
	s, calls := cardSidecarScraper(t, 500000, func(r *http.Request) *http.Response {
		return resp(502, "no healthy lanes", nil)
	})
	s.SetCardDirect(false) // штатный режим с 23-09: публичный путь выключен
	s.ucard = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		return resp(200, wbUCardBody(69900), nil), nil
	})}

	res, err := s.Scrape(context.Background(), "https://www.wildberries.ru/catalog/211695539/detail.aspx")
	if err != nil {
		t.Fatalf("скрейп упал: %v", err)
	}
	if *calls == 0 {
		t.Fatal("сайдкар не пробовали — а он основной путь")
	}
	if res.Price != 699 {
		t.Fatalf("цена %v — публичный хост не подхватил упавший браузер", res.Price)
	}
}
