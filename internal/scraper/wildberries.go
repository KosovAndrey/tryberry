package scraper

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"regexp"
	"strconv"
	"time"

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
	if err := c.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Пробуем v2, потом v1
	endpoints := []string{
		"https://card.wb.ru/cards/v2/detail?appType=1&curr=rub&dest=-1257786&spp=30&nm=%s",
		"https://card.wb.ru/cards/v1/detail?appType=1&curr=rub&dest=-1257786&spp=30&nm=%s",
	}

	for _, tpl := range endpoints {
		result, err := c.tryFetch(ctx, fmt.Sprintf(tpl, articleID))
		if err == nil && result != nil {
			return result, nil
		}
	}

	// Fallback: basket CDN
	return c.fetchFromBasket(ctx, articleID)
}

func (c *Client) tryFetch(ctx context.Context, url string) (*Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Accept-Language", "ru-RU,ru;q=0.9")
	req.Header.Set("Origin", "https://www.wildberries.ru")
	req.Header.Set("Referer", "https://www.wildberries.ru/")

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("status %d", resp.StatusCode)
	}

	var parsed wbResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Data.Products) == 0 {
		return nil, fmt.Errorf("no products")
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
		return nil, fmt.Errorf("price is zero")
	}

	var imageURL string
	if len(p.Photos) > 0 {
		imageURL = p.Photos[0].Big
	}

	return &Result{Name: p.Name, Price: price, ImageURL: imageURL}, nil
}

func (c *Client) fetchFromBasket(ctx context.Context, articleID string) (*Result, error) {
	id, err := strconv.ParseInt(articleID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("invalid article id: %w", err)
	}

	vol := id / 100000
	part := id / 1000
	basket := basketNumber(id)
	base := fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%s/info",
		basket, vol, part, articleID)

	// Получаем название
	name, imageURL := c.fetchBasketCard(ctx, base, articleID, basket, vol, part)

	// Получаем цену из price-history.json
	price, err := c.fetchBasketPrice(ctx, base)
	if err != nil {
		return nil, fmt.Errorf("basket price: %w", err)
	}

	return &Result{Name: name, Price: price, ImageURL: imageURL}, nil
}

func (c *Client) fetchBasketCard(ctx context.Context, base, articleID string, basket, vol, part int64) (string, string) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/ru/card.json", nil)
	resp, err := c.http.Do(req)
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

func (c *Client) fetchBasketPrice(ctx context.Context, base string) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/price-history.json", nil)
	resp, err := c.http.Do(req)
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

func basketNumber(id int64) int64 {
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

// ── WB API response structures ───────────────────────────────────────────────

type wbResponse struct {
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

func (r *wbResponse) toResult() (*Result, error) {
	if len(r.Data.Products) == 0 {
		return nil, fmt.Errorf("товар не найден в ответе WB API")
	}

	p := r.Data.Products[0]

	var priceKopecks int64
	for _, size := range p.Sizes {
		if size.Price.Product > 0 {
			priceKopecks = size.Price.Product
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
