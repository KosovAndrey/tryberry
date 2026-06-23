// Command yandex-probe — разовый эксперимент: проверяет гипотезу «прогретые куки
// через прокси» для market.yandex.ru (задача из трекера). НЕ часть прод-флоу.
//
// Зачем. Сейчас YandexMarketScraper бьёт одним холодным GET сразу по карточке
// (пустой cookie-jar) через RU-прокси — это самый «капчегенный» момент, и каждый
// запрос идёт через прокси (дорого). AliExpress вчера решили схемой direct+proxy
// с общим jar: прокси платим только за разовый прогрев cookie, дальше ходим
// напрямую с датацентр-IP. Этот probe проверяет, работает ли тот же приём для
// Я.Маркета, и печатает сравнительную таблицу по 4 сценариям на каждый URL:
//
//	A cold-direct  — без прогрева, без прокси (датацентр-IP). Baseline: ожидаем капчу.
//	B proxy-cold   — без прогрева, через прокси. Текущее поведение прода.
//	C warm→direct  — прогрев через прокси (homepage), затем карточка БЕЗ прокси.
//	                 ГИПОТЕЗА: если работает — прокси платим только за прогрев.
//	D warm→proxy   — прогрев через прокси, затем карточка через прокси.
//	                 Проверяет, снижает ли сам прогрев частоту капчи на proxy-пути.
//
// Запуск (на VPS, где есть RU-прокси):
//
//	docker build --build-arg SERVICE=yandex-probe -t yandex-probe .
//	docker run --rm -e YANDEX_PROXY_URL="$YANDEX_PROXY_URL" yandex-probe \
//	    "https://market.yandex.ru/card/.../<sku>" "https://market.yandex.ru/product--.../<id>"
//
// Если URL не переданы аргументами — берёт из YANDEX_TEST_URLS (через запятую).
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	homeURL   = "https://market.yandex.ru/"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"
	bodyCap   = 6 << 20 // карточка ~2.5 МБ
)

func main() {
	proxyURL := os.Getenv("YANDEX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = os.Getenv("OZON_PROXY_URL")
	}
	if proxyURL == "" {
		fmt.Fprintln(os.Stderr, "FATAL: YANDEX_PROXY_URL (или OZON_PROXY_URL) не задан — без RU-прокси проверка бессмысленна")
		os.Exit(2)
	}

	urls := os.Args[1:]
	if len(urls) == 0 {
		if env := os.Getenv("YANDEX_TEST_URLS"); env != "" {
			for _, u := range strings.Split(env, ",") {
				if u = strings.TrimSpace(u); u != "" {
					urls = append(urls, u)
				}
			}
		}
	}
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "usage: yandex-probe <card-url> [<card-url>...]   (или env YANDEX_TEST_URLS=url1,url2)")
		os.Exit(2)
	}

	fmt.Printf("proxy: %s\n", maskProxy(proxyURL))
	fmt.Printf("urls:  %d\n\n", len(urls))

	ctx := context.Background()
	for i, target := range urls {
		fmt.Printf("════════ [%d/%d] %s\n", i+1, len(urls), target)
		runScenarios(ctx, proxyURL, target)
		fmt.Println()
	}
}

// outcome — итог одного запроса карточки для строки таблицы.
type outcome struct {
	label    string
	status   int
	blocked  bool   // распознана SmartCaptcha
	product  bool   // в HTML есть JSON-LD Product
	price    string // вытащенная цена (если нашли в JSON-LD)
	bytes    int
	cookies  int    // сколько cookie в jar на момент запроса
	warmInfo string // статус/блок прогрева (для C/D)
	err      string
}

func runScenarios(ctx context.Context, proxyURL, target string) {
	var rows []outcome

	// A: cold-direct — без прогрева, без прокси.
	rows = append(rows, fetchCard(ctx, mkClient(""), "A cold-direct", target))

	// B: proxy-cold — без прогрева, через прокси (текущий прод).
	rows = append(rows, fetchCard(ctx, mkClient(proxyURL), "B proxy-cold", target))

	// C: warm→direct — прогрев через прокси, карточка direct (общий jar).
	{
		jar := tls_client.NewCookieJar()
		proxyCli := mkClientJar(proxyURL, jar)
		directCli := mkClientJar("", jar)
		warm := warmup(ctx, proxyCli)
		time.Sleep(1500 * time.Millisecond) // короткая «человеческая» пауза
		o := fetchCard(ctx, directCli, "C warm→direct", target)
		o.warmInfo = warm
		rows = append(rows, o)
	}

	// D: warm→proxy — прогрев через прокси, карточка через прокси (общий jar).
	{
		jar := tls_client.NewCookieJar()
		proxyCli := mkClientJar(proxyURL, jar)
		warm := warmup(ctx, proxyCli)
		time.Sleep(1500 * time.Millisecond)
		o := fetchCard(ctx, proxyCli, "D warm→proxy", target)
		o.warmInfo = warm
		rows = append(rows, o)
	}

	printTable(rows)
}

// warmup — GET homepage через прокси, чтобы получить антибот/сессионные cookie
// (yandexuid, spravka, _yasc, i, …) в общий jar. Возвращает краткий статус.
func warmup(ctx context.Context, client tls_client.HttpClient) string {
	status, body, names, err := doGet(ctx, client, homeURL, "")
	if err != nil {
		return "warm:ERR " + err.Error()
	}
	blk := ""
	if isCaptcha(body) {
		blk = " CAPTCHA"
	}
	return fmt.Sprintf("warm:%d%s cookies=%d[%s]", status, blk, len(names), strings.Join(names, ","))
}

// fetchCard делает запрос карточки и парсит минимум для оценки.
func fetchCard(ctx context.Context, client tls_client.HttpClient, label, target string) outcome {
	status, body, names, err := doGet(ctx, client, target, homeURL)
	o := outcome{label: label, status: status, bytes: len(body), cookies: len(names)}
	if err != nil {
		o.err = err.Error()
		return o
	}
	o.blocked = isCaptcha(body)
	o.product = strings.Contains(string(body), `"@type":"Product"`) ||
		strings.Contains(string(body), `"@type": "Product"`)
	o.price = firstPrice(body)
	return o
}

// doGet — GET с браузерными заголовками под Chrome-профиль. referer пустой для
// homepage, иначе homepage (как навигация с главной).
func doGet(ctx context.Context, client tls_client.HttpClient, target, referer string) (int, []byte, []string, error) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, target, nil)
	if err != nil {
		return 0, nil, nil, err
	}
	h := fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":           {"ru,en;q=0.9"},
		"sec-ch-ua":                 {`"Chromium";v="148", "Google Chrome";v="148", "Not.A/Brand";v="24"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Linux"`},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-user":            {"?1"},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {userAgent},
	}
	order := []string{
		"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
		"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
	}
	if referer == "" {
		h["sec-fetch-site"] = []string{"none"}
		order = append(order, "sec-fetch-site", "sec-fetch-user", "upgrade-insecure-requests", "user-agent")
	} else {
		h["sec-fetch-site"] = []string{"same-origin"}
		h["referer"] = []string{referer}
		order = append(order, "sec-fetch-site", "sec-fetch-user", "referer", "upgrade-insecure-requests", "user-agent")
	}
	h[fhttp.HeaderOrderKey] = order
	req.Header = h

	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))

	var names []string
	if u, perr := url.Parse(target); perr == nil {
		for _, c := range client.GetCookies(u) {
			names = append(names, c.Name)
		}
	}
	return resp.StatusCode, body, names, nil
}

func mkClient(proxyURL string) tls_client.HttpClient {
	return mkClientJar(proxyURL, tls_client.NewCookieJar())
}

func mkClientJar(proxyURL string, jar tls_client.CookieJar) tls_client.HttpClient {
	opts := []tls_client.HttpClientOption{
		tls_client.WithTimeoutSeconds(30),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(jar),
	}
	if proxyURL != "" {
		opts = append(opts, tls_client.WithProxyUrl(proxyURL))
	}
	c, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(), opts...)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tls-client init:", err)
		os.Exit(1)
	}
	return c
}

// isCaptcha — те же маркеры, что и в боевом isYandexCaptcha: сперва отсекаем
// живую страницу приложения (@marketfront / data-baobab-name="$page").
func isCaptcha(body []byte) bool {
	s := string(body)
	if strings.Contains(s, "@marketfront/") || strings.Contains(s, `data-baobab-name="$page"`) {
		return false
	}
	ls := strings.ToLower(s)
	return strings.Contains(ls, "smartcaptcha") ||
		strings.Contains(ls, "checkbox-captcha") ||
		strings.Contains(ls, "showcaptcha") ||
		strings.Contains(ls, "подтвердите, что запросы отправляли вы")
}

// firstPrice — грубо вытаскивает первую цену из JSON-LD offers (для оценки, что
// данные реально достаём). Не претендует на точность боевого парсера.
func firstPrice(body []byte) string {
	s := string(body)
	i := strings.Index(s, `"application/ld+json"`)
	if i < 0 {
		return ""
	}
	rest := s[i:]
	j := strings.Index(rest, `"price"`)
	if j < 0 {
		return ""
	}
	frag := rest[j+len(`"price"`):]
	frag = strings.TrimLeft(frag, ": \"")
	end := strings.IndexAny(frag, `",}`)
	if end < 0 || end > 20 {
		return ""
	}
	return strings.TrimSpace(frag[:end])
}

func printTable(rows []outcome) {
	fmt.Printf("  %-14s %6s %8s %8s %10s %7s %s\n", "scenario", "status", "blocked", "product", "price", "cookies", "note")
	for _, r := range rows {
		blocked := "no"
		if r.blocked {
			blocked = "YES"
		}
		product := "no"
		if r.product {
			product = "yes"
		}
		price := r.price
		if price == "" {
			price = "—"
		}
		note := r.warmInfo
		if r.err != "" {
			note = "ERR " + r.err
		}
		fmt.Printf("  %-14s %6d %8s %8s %10s %7d  %s\n",
			r.label, r.status, blocked, product, price, r.cookies, note)
	}
}

func maskProxy(p string) string {
	if at := strings.Index(p, "@"); at >= 0 {
		if scheme := strings.Index(p, "://"); scheme >= 0 && scheme < at {
			return p[:scheme+3] + "***@" + p[at+1:]
		}
		return "***@" + p[at+1:]
	}
	return p
}
