package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"time"

	"github.com/cenkalti/backoff/v4"
	"golang.org/x/time/rate"
)

// Result — результат скрейпинга одного товара
type Result struct {
	Name     string
	Price    float64
	ImageURL string
}

type Client struct {
	http    *http.Client
	limiter *rate.Limiter
}

func NewClient(rps float64) *Client {
	return &Client{
		http:    &http.Client{Timeout: 10 * time.Second},
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

// articleRe извлекает числовой артикул из URL Wildberries
// https://www.wildberries.ru/catalog/123456789/detail.aspx → 123456789
var articleRe = regexp.MustCompile(`/catalog/(\d+)/`)

func ExtractArticleID(rawURL string) (string, error) {
	m := articleRe.FindStringSubmatch(rawURL)
	if len(m) < 2 {
		return "", fmt.Errorf("не удалось извлечь артикул из URL: %s", rawURL)
	}
	return m[1], nil
}

// Scrape — получить данные о товаре с Wildberries.
// Соблюдает rate limit и делает до 3 попыток с exponential backoff.
func (c *Client) Scrape(ctx context.Context, articleID string) (*Result, error) {
	var result *Result

	operation := func() error {
		// Соблюдаем rate limit перед каждым запросом
		if err := c.limiter.Wait(ctx); err != nil {
			return backoff.Permanent(err) // контекст отменён — не ретраить
		}

		url := fmt.Sprintf(
			"https://card.wb.ru/cards/v1/detail?appType=1&curr=rub&dest=-1257786&spp=30&nm=%s",
			articleID,
		)

		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			return backoff.Permanent(err)
		}
		req.Header.Set("User-Agent", "Mozilla/5.0")

		resp, err := c.http.Do(req)
		if err != nil {
			return err // временная ошибка — ретраить
		}
		defer resp.Body.Close()

		if resp.StatusCode != http.StatusOK {
			return fmt.Errorf("WB API вернул статус %d", resp.StatusCode)
		}

		var parsed wbResponse
		if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
			return backoff.Permanent(fmt.Errorf("decode: %w", err))
		}

		r, err := parsed.toResult()
		if err != nil {
			return backoff.Permanent(err)
		}
		result = r
		return nil
	}

	b := backoff.WithContext(
		backoff.WithMaxRetries(backoff.NewExponentialBackOff(), 3),
		ctx,
	)

	if err := backoff.Retry(operation, b); err != nil {
		return nil, fmt.Errorf("scrape articleID=%s: %w", articleID, err)
	}
	return result, nil
}

// ── WB API response structures ───────────────────────────────────────────────

type wbResponse struct {
	Data struct {
		Products []struct {
			Name  string `json:"name"`
			Sizes []struct {
				Price struct {
					Total int64 `json:"total"` // в копейках
				} `json:"price"`
			} `json:"sizes"`
			Photos []struct {
				Big string `json:"big"`
			} `json:"photos"`
		} `json:"products"`
	} `json:"data"`
}

func (r *wbResponse) toResult() (*Result, error) {
	if len(r.Data.Products) == 0 {
		return nil, fmt.Errorf("товар не найден в ответе WB API")
	}

	p := r.Data.Products[0]

	// Цена берётся из первого доступного размера
	var priceKopecks int64
	for _, size := range p.Sizes {
		if size.Price.Total > 0 {
			priceKopecks = size.Price.Total
			break
		}
	}
	if priceKopecks == 0 {
		return nil, fmt.Errorf("цена товара равна нулю или не найдена")
	}

	var imageURL string
	if len(p.Photos) > 0 {
		imageURL = p.Photos[0].Big
	}

	return &Result{
		Name:     p.Name,
		Price:    float64(priceKopecks) / 100,
		ImageURL: imageURL,
	}, nil
}
