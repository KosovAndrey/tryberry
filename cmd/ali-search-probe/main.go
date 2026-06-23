// Command ali-search-probe — разовый discovery: где aliexpress.ru отдаёт позиции
// ПОИСКОВОЙ выдачи (SSR-стейт в HTML или отдельный XHR). НЕ часть прод-флоу.
//
// Карточка Ali ходит через /aer-jsonapi productData (см. internal/scraper/
// aliexpress.go). Для выдачи структура неизвестна, и с датацентр-IP без прокси
// X5SEC отдаёт капчу. Probe повторяет ali-транспорт (direct+proxy с общим jar +
// прогрев aer-cookie через прокси) и по странице выдачи печатает:
//   - status / blocked(X5SEC) / размер;
//   - какие из кандидат-контейнеров стейта присутствуют (window.runParams,
//     __INITIAL_DATA__, __AER…, "itemList"/"items"/"productId" и пр.);
//   - сниппет вокруг первого ценового маркера;
//   - PROBE_DUMP=1 → сохранить тело в /tmp/ali-search-<n>.html для ручного разбора.
//
// Запуск на VPS (нужен RU-прокси):
//
//	docker build --build-arg SERVICE=ali-search-probe -t ali-search-probe .
//	docker run --rm --env-file .env -e PROBE_DUMP=1 ali-search-probe \
//	    "https://aliexpress.ru/wholesale?SearchText=футболка&page=1"
package main

import (
	"context"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	aliBase   = "https://aliexpress.ru"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	bodyCap   = 8 << 20
)

func main() {
	proxyURL := os.Getenv("ALI_PROXY_URL")
	if proxyURL == "" {
		proxyURL = os.Getenv("OZON_PROXY_URL")
	}
	if proxyURL == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ALI_PROXY_URL/OZON_PROXY_URL не задан — без RU-прокси X5SEC не пройти")
		os.Exit(2)
	}
	urls := os.Args[1:]
	if len(urls) == 0 {
		urls = []string{aliBase + "/wholesale?SearchText=%D1%84%D1%83%D1%82%D0%B1%D0%BE%D0%BB%D0%BA%D0%B0&page=1"}
	}
	dump := os.Getenv("PROBE_DUMP") == "1"

	jar := tls_client.NewCookieJar()
	direct := mkClient("", jar)
	proxy := mkClient(proxyURL, jar)
	seedLocale(direct, jar)

	ctx := context.Background()
	for i, target := range urls {
		fmt.Printf("════════ [%d/%d] %s\n", i+1, len(urls), target)
		probe(ctx, direct, proxy, target, i, dump)
		fmt.Println()
	}
}

func probe(ctx context.Context, direct, proxy tls_client.HttpClient, target string, idx int, dump bool) {
	// Прогрев: GET страницы выдачи через прокси — проходит X5SEC и кладёт aer-cookie
	// в общий jar (как у карточного скрейпера). Дальше пробуем direct.
	ws, wb := get(ctx, proxy, target)
	fmt.Printf("  warmup(proxy): status=%d blocked=%s len=%d cookies=%d\n",
		ws, yn(isBlocked(wb)), len(wb), cookieCount(proxy, target))
	time.Sleep(1200 * time.Millisecond)

	status, body := get(ctx, direct, target)
	source := "direct"
	if status != 200 || isBlocked(body) {
		status, body = get(ctx, proxy, target)
		source = "proxy"
	}
	fmt.Printf("  fetch(%s): status=%d blocked=%s len=%d\n", source, status, yn(isBlocked(body)), len(body))

	report(body)

	if dump {
		path := fmt.Sprintf("/tmp/ali-search-%d.html", idx)
		if err := os.WriteFile(path, body, 0o644); err == nil {
			fmt.Printf("  dump → %s\n", path)
		}
	}
}

// report ищет известные контейнеры стейта и ценовые маркеры.
func report(body []byte) {
	s := string(body)
	markers := []string{
		"window.runParams", "__INITIAL_DATA__", "window._dida_config_",
		"__AER_DATA__", "__AER_SSR_DATA__", "window.__NUXT__", "__APP_DATA__",
		`"itemList"`, `"items"`, `"mods"`, `"productId"`, `"productIds"`,
		`"mult_minPrice"`, `"minPrice"`, `"salePrice"`, `"formattedPrice"`,
		`"trade"`, `"star"`, `"sku"`,
	}
	fmt.Printf("  markers present:\n")
	for _, m := range markers {
		if n := strings.Count(s, m); n > 0 {
			fmt.Printf("    %-22s ×%d\n", m, n)
		}
	}

	// <script> с id/data-* — где обычно лежит SSR-стейт.
	for _, m := range scriptTagRe.FindAllStringSubmatch(s, 8) {
		fmt.Printf("    <script %s>\n", strings.TrimSpace(m[1]))
	}

	// Сниппет вокруг первого правдоподобного ценового маркера.
	for _, pm := range []string{`"mult_minPrice"`, `"minPrice"`, `"salePrice"`, `"formattedPrice"`, `"price"`} {
		if i := strings.Index(s, pm); i >= 0 {
			end := i + 240
			if end > len(s) {
				end = len(s)
			}
			fmt.Printf("  price ctx (%s):\n    %s\n", pm, collapse(s[i:end]))
			break
		}
	}
}

var scriptTagRe = regexp.MustCompile(`(?i)<script\b([^>]*\b(?:id|data-[a-z-]+)="[^"]*"[^>]*)>`)

func collapse(s string) string {
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.ReplaceAll(s, "\t", " ")
	return strings.Join(strings.Fields(s), " ")
}

func isBlocked(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "x5secdata") || strings.Contains(s, "_____tmd_____") ||
		strings.Contains(s, "punish") || strings.Contains(s, "Slider captcha")
}

func get(ctx context.Context, client tls_client.HttpClient, target string) (int, []byte) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, target, nil)
	if err != nil {
		return 0, nil
	}
	req.Header = fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":           {"ru,en;q=0.9"},
		"sec-ch-ua":                 {`"Chromium";v="146", "Google Chrome";v="146", "Not.A/Brand";v="24"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Windows"`},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-user":            {"?1"},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {userAgent},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
			"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
			"sec-fetch-site", "sec-fetch-user", "upgrade-insecure-requests", "user-agent",
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		return 0, nil
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	return resp.StatusCode, body
}

func mkClient(proxyURL string, jar tls_client.CookieJar) tls_client.HttpClient {
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
		fmt.Fprintln(os.Stderr, "tls-client:", err)
		os.Exit(1)
	}
	return c
}

// seedLocale кладёт locale-cookie рынка RU/RUB (как карточный скрейпер).
func seedLocale(client tls_client.HttpClient, jar tls_client.CookieJar) {
	u, _ := url.Parse(aliBase)
	client.SetCookies(u, []*fhttp.Cookie{{
		Name: "aep_usuc_f", Value: "site=rus&c_tp=RUB&region=RU&b_locale=ru_RU",
		Domain: ".aliexpress.ru", Path: "/",
	}})
}

func cookieCount(client tls_client.HttpClient, target string) int {
	u, err := url.Parse(target)
	if err != nil {
		return 0
	}
	return len(client.GetCookies(u))
}

func yn(b bool) string {
	if b {
		return "YES"
	}
	return "no"
}
