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

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/time/rate"
)

type WildberriesScraper struct {
	http    *http.Client
	limiter *rate.Limiter
}

func NewWildberriesScraper(rps float64) *WildberriesScraper {
	return &WildberriesScraper{
		http:    &http.Client{Timeout: 10 * time.Second},
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

func (s *WildberriesScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	articleID, err := ExtractArticleID(url)
	if err != nil {
		return nil, err
	}

	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	endpoints := []string{
		"https://card.wb.ru/cards/v2/detail?appType=1&curr=rub&dest=-1257786&spp=30&nm=%s",
		"https://card.wb.ru/cards/v1/detail?appType=1&curr=rub&dest=-1257786&spp=30&nm=%s",
	}

	for _, tpl := range endpoints {
		result, err := s.tryFetch(ctx, fmt.Sprintf(tpl, articleID))
		if err == nil && result != nil {
			return result, nil
		}
	}

	// Fallback на basket CDN
	return s.fetchFromBasket(ctx, articleID)
}

func (s *WildberriesScraper) tryFetch(ctx context.Context, url string) (*Result, error) {
	var result *Result

	op := func() error {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
		req.Header.Set("Accept", "*/*")
		req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
		req.Header.Set("Origin", "https://www.wildberries.ru")
		req.Header.Set("Referer", "https://www.wildberries.ru/")

		resp, err := s.http.Do(req)
		if err != nil {
			return err
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("status %d", resp.StatusCode)
		}

		var parsed wbCardResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return backoff.Permanent(err)
		}
		if len(parsed.Data.Products) == 0 {
			return backoff.Permanent(ErrProductNotFound)
		}

		p := parsed.Data.Products[0]
		var price float64
		for _, size := range p.Sizes {
			for _, val := range []int64{size.Price.Product, size.Price.Total, size.Price.Basic} {
				if val > 0 {
					price = float64(val) / 100
					break
				}
			}
			if price > 0 {
				break
			}
		}
		if price == 0 {
			return backoff.Permanent(fmt.Errorf("price is zero"))
		}

		var imageURL string
		if len(p.Photos) > 0 {
			imageURL = p.Photos[0].Big
		}

		result = &Result{Name: p.Name, Price: price, ImageURL: imageURL}
		return nil
	}

	b := backoff.WithContext(backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3), ctx)
	if err := backoff.Retry(op, b); err != nil {
		return nil, err
	}
	return result, nil
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

	name, imageURL := s.fetchBasketCard(ctx, base, articleID, basket, vol, part)
	price, err := s.fetchBasketPrice(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("basket price: %w", err)
	}

	return &Result{Name: name, Price: price, ImageURL: imageURL}, nil
}

func (s *WildberriesScraper) fetchBasketCard(ctx context.Context, base, articleID string, basket, vol, part int64) (string, string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ru/card.json", nil)
	resp, err := s.http.Do(req)
	if err != nil || resp.StatusCode != http.StatusOK {
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

func (s *WildberriesScraper) fetchBasketPrice(ctx context.Context, base string) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/price-history.json", nil)
	resp, err := s.http.Do(req)
	if err != nil {
		return 0, err
	}
	defer resp.Body.Close()

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

// ── Внутренние структуры WB API ──────────────────────────────────────────────

type wbCardResponse struct {
	Data struct {
		Products []struct {
			Name  string `json:"name"`
			Sizes []struct {
				Price struct {
					Basic   int64 `json:"basic"`
					Product int64 `json:"product"`
					Total   int64 `json:"total"`
				} `json:"price"`
			} `json:"sizes"`
			Photos []struct {
				Big string `json:"big"`
			} `json:"photos"`
		} `json:"products"`
	} `json:"data"`
}
