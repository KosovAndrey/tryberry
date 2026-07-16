package scraper

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

// CondEntry — снимок последнего успешного basket-скрейпа: HTTP-валидаторы
// price-history.json (ETag/Last-Modified) + распарсенный результат. Валидаторы
// дают дешёвый change-detection (conditional GET → 304 без тела вместо двух
// полных GET), а снимок позволяет на 304 вернуть полноценный Result, не трогая
// ни card.json, ни даунстрим (notifier получает событие как обычно).
type CondEntry struct {
	ETag         string  `json:"etag,omitempty"`
	LastModified string  `json:"lm,omitempty"`
	Price        float64 `json:"price"`
	Name         string  `json:"name"`
	ImageURL     string  `json:"img"`
}

// CondCache — хранилище CondEntry по артикулу. Реализуется redisrepo.BasketCache.
// Контракт TTL: запись живёт ограниченно и НЕ продлевается при 304 — истечение
// ключа принудительно возвращает товар на полный скрейп (свежие имя/картинка/
// валидаторы), иначе стабильный по цене товар не перечитывался бы никогда.
type CondCache interface {
	GetCond(ctx context.Context, id int64) (CondEntry, bool)
	PutCond(ctx context.Context, id int64, e CondEntry)
}

type WildberriesScraper struct {
	http    *http.Client
	ucard   *http.Client // клиент для u-card-fallback; по умолчанию = http
	limiter *rate.Limiter
	baskets BasketResolver // nilable
	cond    CondCache      // nilable
	// cardBrowserURL — база сайдкара wb-search-miner (/card). Пустая строка →
	// живой карточки нет, цена берётся из архива как раньше (см. SetCardBrowser).
	cardBrowserURL    string
	cardBrowserClient *http.Client
}

func NewWildberriesScraper(rps float64) *WildberriesScraper {
	// Один переиспользуемый клиент на basket-CDN и u-card с тюнингованным пулом
	// keep-alive (per-host 32): basket-шарды — десятки хостов, ходим часто и
	// конкуррентно, дефолтные 2 соединения/хост заставляли бы пересоздавать TLS.
	c := &http.Client{Timeout: 8 * time.Second, Transport: newTunedHTTPTransport()}
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

// SetCondCache подключает кэш снимков для conditional GET (опционально; сервисы
// передают redisrepo.BasketCache). Без него каждый скрейп полный, как раньше.
func (s *WildberriesScraper) SetCondCache(c CondCache) { s.cond = c }

// SetCardBrowser подключает сайдкар wb-search-miner как ОСНОВНОЙ источник цены
// (GET /card?nm=<id> — перехват u-card XHR живой карточки в прогретом браузере).
//
// Зачем: basket-CDN price-history.json отстаёт на ДНИ, и его последняя точка ≠
// текущая цена. Пустой baseURL → старое поведение (цена из архива), т.е. флаг
// одновременно и рубильник отката.
//
// Таймаут щедрый (45с, как у поиска через тот же сайдкар): дорожка выдерживает
// человекоподобный интервал и может ждать навигации — это не обычный HTTP.
func (s *WildberriesScraper) SetCardBrowser(baseURL string) {
	s.cardBrowserURL = strings.TrimRight(baseURL, "/")
	if s.cardBrowserURL == "" {
		return
	}
	s.cardBrowserClient = &http.Client{Timeout: 45 * time.Second, Transport: newTunedHTTPTransport()}
}

// fetchFromBrowserCard — ЖИВАЯ карточка через сайдкар. Форма ответа совпадает с
// u-card/поисковой выдачей, поэтому парсим тем же wbSearchResponse.
//
// Цена 0 при валидной карточке — это НЕ ошибка, а «нет активного оффера»: здесь
// (в отличие от архивной дорожки, которая знать этого не может) мы видим живой
// buy-box, поэтому отдаём InStock=false честно.
func (s *WildberriesScraper) fetchFromBrowserCard(ctx context.Context, articleID string) (*Result, error) {
	if s.cardBrowserURL == "" || s.cardBrowserClient == nil {
		return nil, fmt.Errorf("%w: card browser sidecar not configured", ErrMarketplaceBlocked)
	}
	id, err := strconv.ParseInt(articleID, 10, 64)
	if err != nil {
		return nil, fmt.Errorf("%w: invalid article id", ErrInvalidURL)
	}

	req, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		s.cardBrowserURL+"/card?nm="+articleID, nil)
	resp, err := s.cardBrowserClient.Do(req)
	if err != nil {
		metrics.WBCardFetch.WithLabelValues("error").Inc()
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		// 403 — стена wbaas, 502 — нет живых дорожек. И то и другое лечится сайдкаром
		// (re-warm/relaunch), а не ретраем отсюда: зовущий уйдёт в архив.
		metrics.WBCardFetch.WithLabelValues(wbCardOutcome(resp.StatusCode)).Inc()
		return nil, fmt.Errorf("card sidecar status %d", resp.StatusCode)
	}

	var parsed wbSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		metrics.WBCardFetch.WithLabelValues("error").Inc()
		return nil, err
	}
	if len(parsed.Products) == 0 {
		// Пустой products — карточки нет либо ответ не тот. НЕ трактуем как OOS:
		// иначе сбой формы ответа разом «погасил» бы все товары WB.
		metrics.WBCardFetch.WithLabelValues("empty").Inc()
		return nil, ErrProductNotFound
	}

	p := parsed.Products[0]
	priceKopecks := ucardPriceKopecks(p)
	metrics.WBCardFetch.WithLabelValues("ok").Inc()
	return &Result{
		Name:     firstNonEmpty(p.Name, "Товар WB"),
		Price:    float64(priceKopecks) / 100,
		ImageURL: wbImageURL(id),
		InStock:  priceKopecks > 0,
	}, nil
}

// wbCardOutcome — метка исхода для метрики: 403 (стена) и 502 (нет дорожек) —
// разные болезни с разным лечением, остальное в кучу.
func wbCardOutcome(status int) string {
	switch status {
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusBadGateway:
		return "no_lanes"
	default:
		return "error"
	}
}

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
// Основной источник ЦЕНЫ — живая карточка через браузерный сайдкар
// (SetCardBrowser). basket-CDN price-history.json остаётся источником ИСТОРИИ
// (бэкфилл), имени и картинки, но НЕ текущей цены: архив отстаёт на дни, и его
// последняя точка врала в 97% скрейпов при success rate 100% (успех != правда —
// метриками такое не ловится, только сверкой с живой карточкой).
//
// Браузер не настроен или не смог (стена/нет дорожек) → архив как раньше: цена
// может отставать, но это лучше, чем ничего. Долю такого режима видно по
// wb_price_source{source="basket"}.
func (s *WildberriesScraper) Scrape(ctx context.Context, url string) (*Result, error) {
	articleID, err := ExtractArticleID(url)
	if err != nil {
		return nil, err
	}

	if err := s.limiter.Wait(ctx); err != nil {
		return nil, err
	}

	if s.cardBrowserURL == "" {
		r, source, err := s.scrapeArchive(ctx, articleID)
		if err == nil {
			metrics.WBPriceSource.WithLabelValues(source).Inc()
		}
		return r, err
	}

	// Живая карточка и архив идут ПАРАЛЛЕЛЬНО: браузерная навигация (~1-2с) заведомо
	// дольше basket-CDN (~100мс), так что история достаётся практически бесплатно.
	var (
		live, arch       *Result
		liveErr, archErr error
		source           string
		wg               sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); live, liveErr = s.fetchFromBrowserCard(ctx, articleID) }()
	go func() { defer wg.Done(); arch, source, archErr = s.scrapeArchive(ctx, articleID) }()
	wg.Wait()

	if liveErr != nil {
		if archErr != nil {
			return nil, archErr
		}
		metrics.WBPriceSource.WithLabelValues(source).Inc()
		return arch, nil
	}

	metrics.WBPriceSource.WithLabelValues("browser").Inc()
	if archErr != nil {
		// Нет в basket (трансгран/удалён/шард за окном) — живой карточки достаточно,
		// просто без бэкфилла истории.
		return live, nil
	}
	// Живые цена и наличие поверх архивных имени/картинки и Истории.
	arch.Price = live.Price
	arch.InStock = live.InStock
	if arch.Name == "" {
		arch.Name = live.Name
	}
	if arch.ImageURL == "" {
		arch.ImageURL = live.ImageURL
	}
	return arch, nil
}

// scrapeArchive — прежний путь: basket-CDN, а для отсутствующих в нём
// (трансгран/удалён) real-time u-card. Возвращает ещё и метку источника, чтобы
// метрику инкрементировал вызывающий: только он знает, победил ли браузер.
func (s *WildberriesScraper) scrapeArchive(ctx context.Context, articleID string) (*Result, string, error) {
	// Уже знаем, что товара нет в basket (трансгран/удалён) → сразу u-card, минуя
	// дорогой 25-шардовый перебор (~10с все 404, блокирует консьюмер).
	if id, perr := strconv.ParseInt(articleID, 10, 64); perr == nil && s.baskets != nil && s.baskets.NoBasket(ctx, id) {
		if ur, uerr := s.fetchFromUCard(ctx, articleID); uerr == nil {
			return ur, "ucard", nil
		}
		return nil, "", ErrProductNotFound
	}

	r, err := s.fetchFromBasket(ctx, articleID)
	if err == nil {
		return r, "basket", nil
	}
	// Нет в basket-CDN (трансгран/удалён) → запоминаем (чтобы впредь не перебирать
	// шарды) и пробуем real-time u-card.
	if errors.Is(err, ErrProductNotFound) {
		if id, perr := strconv.ParseInt(articleID, 10, 64); perr == nil && s.baskets != nil {
			s.baskets.MarkNoBasket(ctx, id)
		}
		if ur, uerr := s.fetchFromUCard(ctx, articleID); uerr == nil {
			return ur, "ucard", nil
		}
	}
	return r, "", err
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

	// Снимок прошлого скрейпа для conditional GET читаем один раз (не в tryBasket:
	// проба соседних шардов дёргала бы Redis на каждый кандидат). Без валидаторов
	// или с битой ценой снимок бесполезен — идём полным путём.
	var cond *CondEntry
	if s.cond != nil {
		if e, ok := s.cond.GetCond(ctx, id); ok && e.Price > 0 && (e.ETag != "" || e.LastModified != "") {
			cond = &e
		} else {
			metrics.WBCondGet.WithLabelValues("miss").Inc()
		}
	}

	if s.baskets != nil {
		if cached, ok := s.baskets.Get(ctx, vol); ok {
			if r, err := s.tryBasket(ctx, id, vol, part, articleID, cached, cond); err == nil {
				metrics.WBBasketResolve.WithLabelValues("cache").Inc()
				return r, nil
			}
			// кэш протух / шард переехал → перепробуем формулой+соседями
		}
	}

	for _, basket := range basketCandidates(candidate, 12) {
		r, err := s.tryBasket(ctx, id, vol, part, articleID, basket, cond)
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

// tryBasket — попытка получить цену (+ карточку) с конкретного шарда.
//
// Со снимком прошлого скрейпа (cond != nil) сначала спрашиваем price-history
// условным GET: 304 → цена не менялась, Result восстанавливается из снимка, а
// card.json не запрашивается вовсе — два полных GET превращаются в один ответ
// без тела. Это дешёвый change-detection: на стабильной цене (норма — ради неё
// же волатильностный бэкофф) WB-трафик падает вдвое по запросам и почти в ноль
// по байтам. Здесь пути последовательны (карточка нужна только при 200), зато
// частый случай 304 — один RTT.
//
// Без снимка цену и карточку тянем ПАРАЛЛЕЛЬНО (два независимых запроса) — это
// вдвое срезает латентность скрейпа на горячем пути (резолв из кэша). Для
// проигрышных шардов карточка бежит параллельно с price-404, так что по времени
// не дороже.
func (s *WildberriesScraper) tryBasket(ctx context.Context, id, vol, part int64, articleID string, basket int64, cond *CondEntry) (*Result, error) {
	base := fmt.Sprintf("https://basket-%02d.wbbasket.ru/vol%d/part%d/%s/info",
		basket, vol, part, articleID)

	if cond != nil {
		pr, err := s.fetchBasketPriceHistory(ctx, base, cond)
		if err != nil {
			return nil, err
		}
		if pr.notModified {
			metrics.WBCondGet.WithLabelValues("not_modified").Inc()
			// History пуст: бэкфилл уже случился на полном скрейпе, который
			// и записал этот снимок.
			return &Result{Name: cond.Name, Price: cond.Price, ImageURL: cond.ImageURL, InStock: true}, nil
		}
		metrics.WBCondGet.WithLabelValues("modified").Inc()
		name, imageURL := s.fetchBasketCard(ctx, base, articleID, basket, vol, part)
		s.putCond(ctx, id, pr, name, imageURL)
		return &Result{Name: name, Price: pr.price, ImageURL: imageURL, InStock: true, History: pr.history}, nil
	}

	var (
		pr             *basketPriceResp
		priceErr       error
		name, imageURL string
		wg             sync.WaitGroup
	)
	wg.Add(2)
	go func() { defer wg.Done(); pr, priceErr = s.fetchBasketPriceHistory(ctx, base, nil) }()
	go func() { defer wg.Done(); name, imageURL = s.fetchBasketCard(ctx, base, articleID, basket, vol, part) }()
	wg.Wait()

	if priceErr != nil {
		return nil, priceErr
	}
	s.putCond(ctx, id, pr, name, imageURL)
	return &Result{Name: name, Price: pr.price, ImageURL: imageURL, InStock: true, History: pr.history}, nil
}

// putCond — запомнить снимок успешного скрейпа для будущих conditional GET.
// Без валидаторов или цены запись бессмысленна (нечем спрашивать / нечего
// возвращать на 304) — пропускаем, следующий скрейп снова будет полным.
func (s *WildberriesScraper) putCond(ctx context.Context, id int64, pr *basketPriceResp, name, imageURL string) {
	if s.cond == nil || pr.price <= 0 || (pr.etag == "" && pr.lastModified == "") {
		return
	}
	s.cond.PutCond(ctx, id, CondEntry{
		ETag:         pr.etag,
		LastModified: pr.lastModified,
		Price:        pr.price,
		Name:         name,
		ImageURL:     imageURL,
	})
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

// wbHistoryMaxAge — окно бэкфилла из WB price-history.json. Файл крошечный
// (~40 байт/точка, каданс ~недельный, обычно ≤3 мес), но прошлые точки требуют
// своих месячных партиций — ограничиваем глубину, чтобы не плодить их без меры.
const wbHistoryMaxAge = 180 * 24 * time.Hour

// basketPriceResp — ответ price-history.json: либо notModified (304 на
// conditional GET — цена не менялась), либо распарсенная цена+серия и свежие
// HTTP-валидаторы для следующего conditional GET.
type basketPriceResp struct {
	notModified  bool
	price        float64
	history      []PriceHistoryPoint
	etag         string
	lastModified string
}

// fetchBasketPriceHistory возвращает текущую цену (последняя точка) И всю
// историческую серию из price-history.json для бэкфилла. Серия — точки строго в
// прошлом (моложе wbHistoryMaxAge), отсортированы по времени; нулевые цены
// пропускаем. Текущую точку в History НЕ включаем — её добавит обычный путь.
// cond != nil → conditional GET с валидаторами прошлого ответа: CDN честно
// отвечает 304 без тела, пока файл не перегенерирован (проверено живой пробой
// 2026-07-08). 304 сигналит и «шард верный» — на чужом шарде был бы 404.
func (s *WildberriesScraper) fetchBasketPriceHistory(ctx context.Context, base string, cond *CondEntry) (*basketPriceResp, error) {
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, base+"/price-history.json", nil)
	req.Header.Set("User-Agent", wbUserAgent)
	if cond != nil {
		if cond.ETag != "" {
			req.Header.Set("If-None-Match", cond.ETag)
		}
		if cond.LastModified != "" {
			req.Header.Set("If-Modified-Since", cond.LastModified)
		}
	}
	resp, err := s.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if cond != nil && resp.StatusCode == http.StatusNotModified {
		return &basketPriceResp{notModified: true}, nil
	}
	// 404 на price-history = товара нет в CDN (несуществующий/удалённый артикул)
	if resp.StatusCode == http.StatusNotFound {
		return nil, ErrProductNotFound
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("price-history status %d", resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	price, history, err := parseWBHistory(body, time.Now())
	if err != nil {
		return nil, err
	}
	return &basketPriceResp{
		price:        price,
		history:      history,
		etag:         resp.Header.Get("Etag"),
		lastModified: resp.Header.Get("Last-Modified"),
	}, nil
}

// parseWBHistory разбирает price-history.json: текущая цена = последняя точка;
// History = все предыдущие точки в окне wbHistoryMaxAge с ненулевой ценой. dt —
// unix-секунды. Чистая (без HTTP) — тестируется на реальном фикстуре.
func parseWBHistory(body []byte, now time.Time) (float64, []PriceHistoryPoint, error) {
	var history []struct {
		Dt    int64 `json:"dt"`
		Price struct {
			RUB int64 `json:"RUB"`
		} `json:"price"`
	}
	if err := json.Unmarshal(body, &history); err != nil {
		return 0, nil, err
	}
	if len(history) == 0 {
		return 0, nil, fmt.Errorf("empty price history")
	}

	raw := history[len(history)-1].Price.RUB
	if raw == 0 {
		return 0, nil, fmt.Errorf("price is zero")
	}
	price := float64(raw) / 100

	// История для бэкфилла: все точки кроме последней (она = текущая цена),
	// в пределах окна и с ненулевой ценой.
	cutoff := now.Add(-wbHistoryMaxAge)
	var points []PriceHistoryPoint
	for _, h := range history[:len(history)-1] {
		if h.Dt == 0 || h.Price.RUB == 0 {
			continue
		}
		at := time.Unix(h.Dt, 0).UTC()
		if at.Before(cutoff) {
			continue
		}
		points = append(points, PriceHistoryPoint{At: at, Price: float64(h.Price.RUB) / 100})
	}
	return price, points, nil
}

const wbUserAgent = "Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36"

// wbUCardBase — открытый real-time эндпоинт карточки (цена в price.product).
const wbUCardBase = "https://u-card.wb.ru/cards/v4/list"

// wbBasketNumber возвращает номер CDN-шарда basket-NN.wbbasket.ru для nmID.
// vol = id/100000; товар отдаёт 200 только на своём шарде, на чужих 404.
//
// Границы vol>6437 измерены пробой живых high-vol артикулов 2026-07-07
// (см. experiments/wb-basket-probe/, сырьё в results.txt). Старая хвостовая
// формула 32+(vol-6438)/312 мазала до +4 на топовых vol (vol 10961 давала 46,
// реально 42), т.к. период 312 сильно занижен: реальная ширина шарда на верхах
// ~430..800 vol/шард. Измеренные нижние границы шардов (min живой vol):
//
//	32:6700 33:6875 34:7068 35:7584 36:7825 37:8015 38:8326
//	39:8844 40:9429 41:9780 42:10961 43:11420 (=макс. живой vol на 2026-07-07).
//
// thresholds[N] = (min vol шарда N+1)-1 → «дырки» без живых id падают в НИЖНИЙ
// шард (недолёт номера безопаснее: runtime добирает соседей ±12 + кэш probe_far).
// Хвост за basket-43 (vol>11419, пока не существует) восстановлен консервативным
// периодом 600 (>средних ~570, чтобы недобирать номер, не перебирать).
func wbBasketNumber(id int64) int64 {
	vol := id / 100000
	thresholds := []int64{
		143, 287, 431, 719, 1007, 1061, 1115, 1169, 1313, 1601,
		1655, 1919, 2045, 2189, 2405, 2621, 2837, 3053, 3269, 3485,
		3701, 3917, 4133, 4349, 4565, 4877, 5189, 5501, 5813, 6125, 6437,
		// измерено 2026-07-07 (шарды 32..42):
		6874, 7067, 7583, 7824, 8014, 8325, 8843, 9428, 9779, 10960, 11419,
	}
	for i, t := range thresholds {
		if vol <= t {
			return int64(i + 1)
		}
	}
	return 43 + (vol-11420)/600
}
