package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/cookiejar"
	"regexp"
	"strings"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/time/rate"
)

type YandexMarketScraper struct {
	http    *http.Client
	limiter *rate.Limiter
}

func NewYandexMarketScraper(rps float64) *YandexMarketScraper {
	jar, _ := cookiejar.New(nil)
	return &YandexMarketScraper{
		http: &http.Client{
			Timeout: 15 * time.Second,
			Jar:     jar,
		},
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

func (s *YandexMarketScraper) Marketplace() Marketplace {
	return MarketplaceYandexMarket
}

func (s *YandexMarketScraper) Matches(url string) bool {
	return strings.Contains(url, "market.yandex.ru/")
}

var ymJSONLDRe = regexp.MustCompile(`(?s)<script type="application/ld\+json"[^>]*>(.*?)</script>`)

func (s *YandexMarketScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	var result *Result

	op := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8")
		req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")

		resp, err := s.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}

		// Стримим ответ блоками — HTML страница ~2.5 МБ, парсить регуляркой
		// всё целиком — много памяти. Регулярка ищет в строке всё равно,
		// поэтому читаем в буфер ограниченного размера.
		buf := make([]byte, 0, 1024*1024)
		tmp := make([]byte, 32*1024)
		for {
			n, rerr := resp.Body.Read(tmp)
			if n > 0 {
				buf = append(buf, tmp[:n]...)
			}
			if rerr != nil {
				break
			}
			if len(buf) > 5*1024*1024 { // защита от слишком больших ответов
				return backoff.Permanent(fmt.Errorf("response too large"))
			}
		}

		r, err := parseYandexMarketHTML(string(buf))
		if err != nil {
			return backoff.Permanent(err)
		}
		result = r
		return nil
	}

	b := backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3), ctx)
	if err := backoff.Retry(op, b); err != nil {
		return nil, fmt.Errorf("scrape yandex market: %w", err)
	}
	return result, nil
}

func parseYandexMarketHTML(html string) (*Result, error) {
	matches := ymJSONLDRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		return nil, ErrProductNotFound
	}

	for _, m := range matches {
		var product ymJSONLDProduct
		if err := json.Unmarshal([]byte(m[1]), &product); err != nil {
			continue
		}
		if product.Type != "Product" || product.Offers.Price == "" {
			continue
		}

		price, err := parsePriceString(product.Offers.Price)
		if err != nil {
			continue
		}

		return &Result{
			Name:     product.Name,
			Price:    price,
			ImageURL: product.Image,
		}, nil
	}

	return nil, ErrProductNotFound
}

// parsePriceString парсит цену из строки в float64.
// JSON-LD может отдавать "22002" или "22002.00".
func parsePriceString(s string) (float64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, fmt.Errorf("empty price")
	}
	var price float64
	if _, err := fmt.Sscanf(s, "%f", &price); err != nil {
		return 0, err
	}
	if price <= 0 {
		return 0, fmt.Errorf("non-positive price: %v", price)
	}
	return price, nil
}

// ── JSON-LD structure ────────────────────────────────────────────────────────

type ymJSONLDProduct struct {
	Type   string `json:"@type"`
	Name   string `json:"name"`
	Image  string `json:"image"`
	Offers struct {
		Price string `json:"price"`
	} `json:"offers"`
}
