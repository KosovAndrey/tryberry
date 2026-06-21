package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	"golang.org/x/time/rate"
)

type WildberriesScraper struct {
	http    *http.Client
	limiter *rate.Limiter
}

func NewWildberriesScraper(rps float64) *WildberriesScraper {
	return &WildberriesScraper{
		// basket CDN отвечает за доли секунды; 8s — щедрый потолок на случай
		// сетевых задержек, но при норме мы укладываемся в <1s
		http:    &http.Client{Timeout: 8 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

func (s *WildberriesScraper) Marketplace() Marketplace {
	return MarketplaceWildberries
}

func (s *WildberriesScraper) Matches(url string) bool {
	return strings.Contains(url, "wildberries.ru/catalog/")
}

var wbArticleRe = regexp.MustCompile(`/catalog/(\d+)/`)

// ExtractArticleID — публичная функция, может пригодиться извне (например, в боте)
func ExtractArticleID(url string) (string, error) {
	m := wbArticleRe.FindStringSubmatch(url)
	if len(m) < 2 {
		return "", fmt.Errorf("%w: not a wildberries product URL", ErrInvalidURL)
	}
	return m[1], nil
}

// Scrape получает данные о товаре напрямую из basket CDN Wildberries.
//
// Раньше код сначала дёргал card.wb.ru/cards/{v1,v2}/detail — но эти endpoint'ы
// отдают 404 (API мёртв/изменился), и backoff крутил их по 3 раза каждый перед
// fallback на basket → ~5s впустую на каждом скрейпе. Теперь basket — основной
// и единственный путь: он быстрый (<1s), детерминированный (URL вычисляется из
// article_id) и стабильный.
//
// Примечание: цена берётся из price-history.json (последняя запись). Она может
// отставать от реальной цены на сайте на несколько часов — это компромисс,
// т.к. real-time источник (card.wb.ru) больше недоступен.
func (s *WildberriesScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	articleID, err := ExtractArticleID(url)
	if err != nil {
		return nil, err
	}

	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	return s.fetchFromBasket(ctx, articleID)
}

func (s *WildberriesScraper) fetchFromBasket(ctx context.Context, articleID string) (*Result, error) {
	id, err := strconv.ParseInt(articleID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid article id", ErrInvalidURL)
	}

	vol := id / 100000
	part := id / 1000
	basket := wbBasketNumber(id)
	base := fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%s/info",
		basket, vol, part, articleID)

	// Цена — обязательна. Если её нет, товар считаем не найденным.
	price, err := s.fetchBasketPrice(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("basket price: %w", err)
	}

	// Имя и картинка — желательны, но не критичны (best-effort)
	name, imageURL := s.fetchBasketCard(ctx, base, articleID, basket, vol, part)

	return &Result{Name: name, Price: price, ImageURL: imageURL, InStock: true}, nil
}

func (s *WildberriesScraper) fetchBasketCard(ctx context.Context, base, articleID string, basket, vol, part int64) (string, string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ru/card.json", nil)
	req.Header.Set("User-Agent", wbUserAgent)
	resp, err := s.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
		if resp != nil {
			resp.Body.Close()
		}
		return "Товар WB", ""
	}
	defer resp.Body.Close()

	var card struct {
		Name string `json:"imt_name"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&card); err != nil {
		return "Товар WB", ""
	}

	imageURL := fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%s/images/big/1.webp",
		basket, vol, part, articleID)
	return card.Name, imageURL
}

// getWithRetry — GET с ретраями ТРАНЗИЕНТНЫХ сбоев (сетевые ошибки/EOF, 5xx). WB
// basket-CDN изредка рвёт соединение (EOF) — один-два ретрая убирают ложные «не
// удалось», особенно в bulk, где нет ручного повтора. 404/2xx — окончательный ответ
// (товара нет / есть), не ретраим. (Несуществующий шард — напр. трансгран. Ali не в
// баскетах — даст устойчивую сетевую ошибку и честно вернётся после ретраев.)
func (s *WildberriesScraper) getWithRetry(ctx context.Context, url string) (*http.Response, error) {
	const attempts = 3
	var lastErr error
	for i := 0; i < attempts; i++ {
		req, _ := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		req.Header.Set("User-Agent", wbUserAgent)
		resp, err := s.http.Do(req)
		switch {
		case err != nil:
			lastErr = err
		case resp.StatusCode >= 500:
			lastErr = fmt.Errorf("status %d", resp.StatusCode)
			resp.Body.Close()
		default:
			return resp, nil // 2xx/3xx/4xx — окончательный ответ
		}
		if i < attempts-1 {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-time.After(time.Duration(200*(i+1)) * time.Millisecond):
			}
		}
	}
	return nil, lastErr
}

func (s *WildberriesScraper) fetchBasketPrice(ctx context.Context, base string) (float64, error) {
	resp, err := s.getWithRetry(ctx, base+"/price-history.json")
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

	// 404 на price-history = товара нет в CDN (несуществующий/удалённый артикул)
	if resp.StatusCode == http.StatusNotFound {
		return 0, ErrProductNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("price-history status %d", resp.StatusCode)
	}

	var history []struct {
		Price struct {
			RUB int64 `json:"RUB"`
		} `json:"price"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&history); err != nil {
		return 0, err
	}
	if len(history) == 0 {
		return 0, fmt.Errorf("empty price history")
	}

	raw := history[len(history)-1].Price.RUB
	if raw == 0 {
		return 0, fmt.Errorf("price is zero")
	}
	return float64(raw) / 100, nil
}

const wbUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

func wbBasketNumber(id int64) int64 {
	vol := id / 100000
	thresholds := []int64{
		143, 287, 431, 719, 1007, 1061, 1115, 1169, 1313, 1601,
		1655, 1919, 2045, 2189, 2405, 2621, 2837, 3053, 3269, 3485,
		3701, 3917, 4133, 4349, 4565, 4877, 5189, 5501, 5813, 6125, 6437,
	}
	for i, t := range thresholds {
		if vol <= t {
			return int64(i + 1)
		}
	}
	return 32 + (vol-6438)/312
}
