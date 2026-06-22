package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	neturl "net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"golang.org/x/time/rate"

	"gitlab.com/KosovAndrey/tryberrybot/internal/metrics"
)

// BasketResolver — кэш соответствия vol→basket-шард (vol = id/100000) плюс
// негатив-кэш «товара нет в basket» (трансгран/удалён). Реализуется
// redisrepo.BasketCache. nil → кэш не используется (только проба формулы+соседей).
type BasketResolver interface {
	Get(ctx context.Context, vol int64) (int64, bool)
	Put(ctx context.Context, vol, basket int64)
	// NoBasket/MarkNoBasket — товара нет ни в одном basket-шарде: помним, чтобы
	// не перебирать 25 шардов на каждом скрейпе, а сразу идти в u-card.
	NoBasket(ctx context.Context, id int64) bool
	MarkNoBasket(ctx context.Context, id int64)
}

type WildberriesScraper struct {
	http    *http.Client
	ucard   *http.Client // клиент для u-card-fallback; по умолчанию = http
	limiter *rate.Limiter
	baskets BasketResolver // nilable
}

func NewWildberriesScraper(rps float64) *WildberriesScraper {
	c := &http.Client{Timeout: 8 * time.Second}
	return &WildberriesScraper{
		// basket CDN отвечает за доли секунды; 8s — щедрый потолок на случай
		// сетевых задержек, но при норме мы укладываемся в <1s
		http:    c,
		ucard:   c,
		limiter: rate.NewLimiter(rate.Limit(rps), 1),
	}
}

// SetBasketResolver подключает кэш vol→basket (опционально; сервисы передают
// redisrepo.BasketCache). Без него скрейпер всё равно работает — пробой формулы+соседей.
func (s *WildberriesScraper) SetBasketResolver(r BasketResolver) { s.baskets = r }

// SetUCardProxy направляет запросы u-card-fallback через прокси. Нужно там, где
// прямой egress 403-ится антиботом u-card (датацентровый RU-IP воркера): прокси
// с зарубежным/чистым выходом (напр. xray) запрос принимает. Пустой URL — оставить
// дефолтный клиент (он сам может ходить через HTTPS_PROXY, как у бота).
func (s *WildberriesScraper) SetUCardProxy(proxyURL string) error {
	if proxyURL == "" {
		return nil
	}
	u, err := neturl.Parse(proxyURL)
	if err != nil {
		return fmt.Errorf("ucard proxy url: %w", err)
	}
	s.ucard = &http.Client{
		Timeout:   8 * time.Second,
		Transport: &http.Transport{Proxy: http.ProxyURL(u)},
	}
	return nil
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

// Scrape получает данные о товаре Wildberries.
//
// Основной источник — basket-CDN price-history.json: быстрый, доступен с прямого
// egress, без антибота. Цена может отставать на часы — компромисс ради простоты.
//
// Fallback — u-card.wb.ru/cards/v4/list (real-time): включается, ТОЛЬКО когда
// товара нет в basket-CDN (удалён / трансграничный «Находки из Китая»). u-card
// 403-ит датацентровый RU-IP, поэтому fallback ходит через прокси (SetUCardProxy,
// напр. xray) — но лишь для редких трансграничных, нагрузка на прокси минимальна.
func (s *WildberriesScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	articleID, err := ExtractArticleID(url)
	if err != nil {
		return nil, err
	}

	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	// Уже знаем, что товара нет в basket (трансгран/удалён) → сразу u-card, минуя
	// дорогой 25-шардовый перебор (~10с все 404, блокирует консьюмер).
	if id, perr := strconv.ParseInt(articleID, 10, 64); perr == nil && s.baskets != nil && s.baskets.NoBasket(ctx, id) {
		if ur, uerr := s.fetchFromUCard(ctx, articleID); uerr == nil {
			metrics.WBPriceSource.WithLabelValues("ucard").Inc()
			return ur, nil
		}
		return nil, ErrProductNotFound
	}

	r, err := s.fetchFromBasket(ctx, articleID)
	if err == nil {
		metrics.WBPriceSource.WithLabelValues("basket").Inc()
		return r, nil
	}
	// Нет в basket-CDN (трансгран/удалён) → запоминаем (чтобы впредь не перебирать
	// шарды) и пробуем real-time u-card.
	if errors.Is(err, ErrProductNotFound) {
		if id, perr := strconv.ParseInt(articleID, 10, 64); perr == nil && s.baskets != nil {
			s.baskets.MarkNoBasket(ctx, id)
		}
		if ur, uerr := s.fetchFromUCard(ctx, articleID); uerr == nil {
			metrics.WBPriceSource.WithLabelValues("ucard").Inc()
			return ur, nil
		}
	}
	return r, err
}

// fetchFromUCard берёт карточку с u-card.wb.ru/cards/v4/list — real-time эндпоинт.
// Форма ответа совпадает с поисковой выдачей (products[].sizes[].price), поэтому
// переиспользуем wbSearchResponse. Ходит через s.ucard (может быть с прокси, см.
// SetUCardProxy — u-card 403-ит прямой RU-IP).
func (s *WildberriesScraper) fetchFromUCard(ctx context.Context, articleID string) (*Result, error) {
	id, err := strconv.ParseInt(articleID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid article id", ErrInvalidURL)
	}

	apiURL := wbUCardBase + "?appType=1&curr=rub&dest=-1257786&spp=30&nm=" + articleID
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	req.Header.Set("User-Agent", wbUserAgent)
	resp, err := s.ucard.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("u-card status %d", resp.StatusCode)
	}

	var parsed wbSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return nil, err
	}
	if len(parsed.Products) == 0 {
		return nil, ErrProductNotFound
	}

	p := parsed.Products[0]
	priceKopecks := ucardPriceKopecks(p)
	if priceKopecks == 0 {
		// Нет активного оффера в u-card → отдаём на fallback (basket price-history
		// мог сохранить последнюю цену). Так контракт WB-скрейпера не меняется:
		// он, как и раньше, не отдаёт «нет в наличии» (InStock всегда true).
		return nil, ErrProductNotFound
	}
	return &Result{
		Name:     firstNonEmpty(p.Name, "Товар WB"),
		Price:    float64(priceKopecks) / 100,
		ImageURL: wbImageURL(id),
		InStock:  true,
	}, nil
}

// ucardPriceKopecks — финальная цена позиции в копейках: product (что видит
// юзер), фолбэк total/basic. 0, если оффера нет.
func ucardPriceKopecks(p wbSearchProduct) int64 {
	if len(p.Sizes) == 0 {
		return 0
	}
	pr := p.Sizes[0].Price
	switch {
	case pr.Product > 0:
		return pr.Product
	case pr.Total > 0:
		return pr.Total
	default:
		return pr.Basic
	}
}

func firstNonEmpty(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}

func (s *WildberriesScraper) fetchFromBasket(ctx context.Context, articleID string) (*Result, error) {
	id, err := strconv.ParseInt(articleID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid article id", ErrInvalidURL)
	}

	vol := id / 100000
	part := id / 1000

	// Номер basket-шарда у WB — лукап-таблица, которую они постоянно расширяют; формула
	// wbBasketNumber для высоких vol мажет (проверено: vol8943 реально на basket-39,
	// формула даёт 40 → стабильный 404). Резолвим так:
	//   1) кэш vol→basket (выучен прежней пробой, неделя) — 1 запрос, без перебора;
	//   2) проба: кандидат формулы → соседи ±12, первый 200 побеждает, кэшируем;
	//   3) нигде не нашли → not_found (удалён / трансгран. Ali не в CDN / шард за окном).
	// Метрика wb_basket_resolve_total{outcome} → видно дрейф формулы и всплески not_found.
	candidate := wbBasketNumber(id)

	if s.baskets != nil {
		if cached, ok := s.baskets.Get(ctx, vol); ok {
			if r, err := s.tryBasket(ctx, vol, part, articleID, cached); err == nil {
				metrics.WBBasketResolve.WithLabelValues("cache").Inc()
				return r, nil
			}
			// кэш протух / шард переехал → перепробуем формулой+соседями
		}
	}

	for _, basket := range basketCandidates(candidate, 12) {
		r, err := s.tryBasket(ctx, vol, part, articleID, basket)
		if err != nil {
			continue // 404 (не тот шард) / сетевая / нет хоста → следующий
		}
		dist := basket - candidate
		if dist < 0 {
			dist = -dist
		}
		switch {
		case dist == 0:
			metrics.WBBasketResolve.WithLabelValues("formula").Inc()
		case dist <= 4:
			metrics.WBBasketResolve.WithLabelValues("probe").Inc()
		default:
			metrics.WBBasketResolve.WithLabelValues("probe_far").Inc() // формула сильно уехала
		}
		if s.baskets != nil {
			s.baskets.Put(ctx, vol, basket)
		}
		return r, nil
	}
	metrics.WBBasketResolve.WithLabelValues("not_found").Inc()
	return nil, fmt.Errorf("basket price: %w", ErrProductNotFound)
}

// tryBasket — попытка получить цену (+ карточку) с конкретного шарда. Цену и
// карточку тянем ПАРАЛЛЕЛЬНО (два независимых запроса) — это вдвое срезает
// латентность скрейпа на горячем пути (резолв из кэша), а значит и пропускную
// последовательного консьюмера. Для проигрышных шардов карточка бежит параллельно
// с price-404, так что по времени не дороже.
func (s *WildberriesScraper) tryBasket(ctx context.Context, vol, part int64, articleID string, basket int64) (*Result, error) {
	base := fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%s/info",
		basket, vol, part, articleID)

	var (
		price          float64
		priceErr       error
		name, imageURL string
		wg             sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); price, priceErr = s.fetchBasketPrice(ctx, base) }()
	go func() { defer wg.Done(); name, imageURL = s.fetchBasketCard(ctx, base, articleID, basket, vol, part) }()
	wg.Wait()

	if priceErr != nil {
		return nil, priceErr
	}
	return &Result{Name: name, Price: price, ImageURL: imageURL, InStock: true}, nil
}

// basketCandidates — порядок проб номеров шарда вокруг кандидата формулы: кандидат
// первым (для известных диапазонов он точен → один шард), затем ближайшие соседи до
// ±maxDelta. Номера <1 отбрасываем.
func basketCandidates(c, maxDelta int64) []int64 {
	out := make([]int64, 0, 2*maxDelta+1)
	seen := map[int64]bool{}
	add := func(b int64) {
		if b >= 1 && !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	add(c)
	for d := int64(1); d <= maxDelta; d++ {
		add(c - d)
		add(c + d)
	}
	return out
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

func (s *WildberriesScraper) fetchBasketPrice(ctx context.Context, base string) (float64, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/price-history.json", nil)
	req.Header.Set("User-Agent", wbUserAgent)
	resp, err := s.http.Do(req)
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

// wbUCardBase — открытый real-time эндпоинт карточки (цена в price.product).
const wbUCardBase = "https://u-card.wb.ru/cards/v4/list"

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
