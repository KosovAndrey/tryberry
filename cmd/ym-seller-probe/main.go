// Command ym-seller-probe — discovery витрины продавца Я.Маркета.
//
// Витрина = SSR-страница маркетфронта (как обычная выдача). Проверяем, какой URL
// реально отдаёт товары продавца и парсятся ли они тем же якорем, что в
// yandex_market_search.parseSearch ({"id":N,"entity":"product"...}):
//   - /business--<slug>/<id>
//   - /search?generalContext=t=merchant;mrch=<id>;
//   - и т.п. (передаём URL'ы аргументами).
//
// Транспорт как у YM-карточки: Chrome-tls-client, direct + proxy-fallback на капчу.
//
//	docker build --build-arg SERVICE=ym-seller-probe -t ym-seller-probe .
//	docker run --rm --env-file .env ym-seller-probe "<url1>" "<url2>"
package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"

// ymProductStartRe — тот же якорь товарной модели, что в yandex_market_search.go.
var ymProductStartRe = regexp.MustCompile(`\{"id":\d+,"entity":"product"`)

var (
	titleRe     = regexp.MustCompile(`(?s)<title>(.*?)</title>`)
	canonicalRe = regexp.MustCompile(`<link[^>]+rel="canonical"[^>]+href="([^"]+)"`)
	ogTitleRe   = regexp.MustCompile(`<meta[^>]+property="og:title"[^>]+content="([^"]+)"`)
	ogSiteRe    = regexp.MustCompile(`<meta[^>]+property="og:site_name"[^>]+content="([^"]+)"`)
)

func main() {
	proxyURL := os.Getenv("YANDEX_PROXY_URL")
	if proxyURL == "" {
		proxyURL = os.Getenv("OZON_PROXY_URL")
	}
	urls := os.Args[1:]
	if len(urls) == 0 {
		fmt.Fprintln(os.Stderr, "usage: ym-seller-probe <url> [<url>...]")
		os.Exit(2)
	}

	jar := tls_client.NewCookieJar()
	direct := mkClient("", jar)
	var proxy tls_client.HttpClient
	if proxyURL != "" {
		proxy = mkClient(proxyURL, jar)
	}

	ctx := context.Background()
	for i, u := range urls {
		fmt.Printf("════════ [%d/%d] %s\n", i+1, len(urls), u)
		status, body, finalURL := get(ctx, direct, u)
		src := "direct"
		if proxy != nil && (status != 200 || isCaptcha(body)) {
			status, body, finalURL = get(ctx, proxy, u)
			src = "proxy"
		}
		models := ymProductStartRe.FindAllStringIndex(string(body), -1)
		fmt.Printf("  %s: status=%d captcha=%s len=%d product-models=%d\n",
			src, status, yn(isCaptcha(body)), len(body), len(models))

		// Источники имени: финальный URL (после редиректа — там реальный слаг),
		// canonical и og-теги.
		fmt.Printf("  final URL: %s\n", finalURL)
		for name, re := range map[string]*regexp.Regexp{
			"<title>":      titleRe,
			"canonical":    canonicalRe,
			"og:title":     ogTitleRe,
			"og:site_name": ogSiteRe,
		} {
			if m := re.FindStringSubmatch(string(body)); len(m) == 2 {
				fmt.Printf("  %-12s %s\n", name+":", strings.TrimSpace(m[1]))
			}
		}
		fmt.Println()
	}
}

func get(ctx context.Context, client tls_client.HttpClient, target string) (int, []byte, string) {
	req, err := fhttp.NewRequestWithContext(ctx, fhttp.MethodGet, target, nil)
	if err != nil {
		return 0, nil, ""
	}
	req.Header = fhttp.Header{
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {userAgent},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent"},
	}
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		return 0, nil, ""
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	finalURL := target
	if resp.Request != nil && resp.Request.URL != nil {
		finalURL = resp.Request.URL.String()
	}
	return resp.StatusCode, b, finalURL
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

func isCaptcha(body []byte) bool {
	s := string(body)
	if strings.Contains(s, "@marketfront/") || strings.Contains(s, `data-baobab-name="$page"`) {
		return false
	}
	ls := strings.ToLower(s)
	return strings.Contains(ls, "smartcaptcha") || strings.Contains(ls, "showcaptcha") ||
		strings.Contains(ls, "checkbox-captcha")
}

func yn(b bool) string {
	if b {
		return "YES"
	}
	return "no"
}
