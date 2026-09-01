// Command ym-form-probe — разовая разведка: какая форма URL карточки Я.Маркета
// сейчас живая и как из голого id получить рабочую ссылку. НЕ часть прод-флоу.
//
// Зачем. 01-09-2026 выяснилось, что с живого RU-адреса (мобильный МегаФон, проверен
// ym-proxy-check) карточка открывается только формой /card/<slug>/<id>, а наш канон
// /product/<id> (миграции 030/031 от 16-07) отдаёт 13 КБ SmartCaptcha. В базе 14086
// ссылок закрытой формы против 15 живой — то есть весь Я.Маркет скрейпит адрес,
// который площадка больше не отдаёт, и никакой прокси этого не лечит.
//
// Что печатает по каждой форме: статус БЕЗ следования редиректам, Location, размер
// тела, капча или нет, и — главное — найденный в теле канонический адрес
// (rel=canonical / og:url / любое вхождение /card/<slug>/<id>). Если фиктивный слаг
// достаточно, чтобы страница отдала настоящий, починка сводится к переписыванию URL
// без похода в выдачу.
//
// Запуск:
//
//	docker build --build-arg SERVICE=ym-form-probe -t ym-form-probe .
//	docker run --rm -e YM_PROXY="socks5://user:pass@ip:port" ym-form-probe [id ...]
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"regexp"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

// Формы одного и того же товара: %s — числовой id.
var forms = []struct{ name, pattern string }{
	{"canon /product/<id>", "https://market.yandex.ru/product/%s"},
	{"old /product--x/<id>", "https://market.yandex.ru/product--x/%s"},
	{"card /card/x/<id>", "https://market.yandex.ru/card/x/%s"},
}

var defaultIDs = []string{"923133126", "666135591", "1170357504"}

var (
	canonicalRe = regexp.MustCompile(`<link[^>]+rel="canonical"[^>]+href="([^"]+)"`)
	ogURLRe     = regexp.MustCompile(`<meta[^>]+property="og:url"[^>]+content="([^"]+)"`)
	titleRe     = regexp.MustCompile(`(?s)<title[^>]*>(.*?)</title>`)
)

func main() {
	proxy := os.Getenv("YM_PROXY")
	if proxy == "" {
		fmt.Fprintln(os.Stderr, "FATAL: задай YM_PROXY, например http://user:pass@ip:port")
		os.Exit(2)
	}
	ids := os.Args[1:]
	if len(ids) == 0 {
		ids = defaultIDs
	}

	// Без следования редиректам: нас интересует именно первый ответ и Location.
	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(40),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithNotFollowRedirects(),
		tls_client.WithProxyUrl(proxy),
	)
	if err != nil {
		fmt.Println("прокси не принят клиентом:", err)
		os.Exit(1)
	}

	// Боевой скрейпер — проверить найденный канонический адрес на реальную цену.
	live := scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
		ProxyURL: proxy, ProxyPrimary: true, RPS: 1,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	for _, id := range ids {
		fmt.Printf("\n═══ id %s ═══\n", id)
		for _, f := range forms {
			u := fmt.Sprintf(f.pattern, id)
			st, body, loc, err := get(client, u)
			if err != nil {
				fmt.Printf("  %-22s ошибка: %v\n", f.name, err)
				continue
			}
			mark := "чисто"
			if isCaptcha(body) {
				mark = "КАПЧА"
			}
			fmt.Printf("  %-22s статус %d, %d КБ, %s\n", f.name, st, len(body)/1024, mark)
			if loc != "" {
				fmt.Printf("  %-22s → Location: %s\n", "", loc)
			}
			if t := first(titleRe, body); t != "" {
				fmt.Printf("  %-22s   title: %s\n", "", trim(t, 90))
			}
			for label, cand := range map[string]string{
				"canonical": first(canonicalRe, body),
				"og:url":    first(ogURLRe, body),
				"в теле":    firstCardLink(body, id),
			} {
				if cand != "" {
					fmt.Printf("  %-22s   %s: %s\n", "", label, cand)
				}
			}
		}
	}

	// Если хоть где-то нашёлся настоящий /card/<slug>/<id> — прогнать боевым скрейпером.
	fmt.Println("\nбоевой скрейпер по найденным каноническим адресам:")
	for _, id := range ids {
		u := fmt.Sprintf("https://market.yandex.ru/card/x/%s", id)
		_, body, _, err := get(client, u)
		if err != nil {
			continue
		}
		real := firstNonEmpty(firstCardLink(body, id), first(canonicalRe, body), first(ogURLRe, body))
		if real == "" {
			fmt.Printf("  id %s: канонический адрес в теле не найден\n", id)
			continue
		}
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		res, err := live.Scrape(ctx, real)
		cancel()
		if err != nil {
			fmt.Printf("  ❌ %-58s %v\n", trim(real, 58), err)
			continue
		}
		fmt.Printf("  ✅ %-58s %9.2f₽ в наличии=%v\n", trim(real, 58), res.Price, res.InStock)
	}
}

func get(c tls_client.HttpClient, u string) (int, []byte, string, error) {
	req, err := fhttp.NewRequest("GET", u, nil)
	if err != nil {
		return 0, nil, "", err
	}
	req.Header = fhttp.Header{
		"accept":                    {"text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,image/webp,*/*;q=0.8"},
		"accept-language":           {"ru,en;q=0.9"},
		"sec-ch-ua":                 {`"Chromium";v="148", "Google Chrome";v="148", "Not.A/Brand";v="24"`},
		"sec-ch-ua-mobile":          {"?0"},
		"sec-ch-ua-platform":        {`"Linux"`},
		"sec-fetch-dest":            {"document"},
		"sec-fetch-mode":            {"navigate"},
		"sec-fetch-site":            {"none"},
		"sec-fetch-user":            {"?1"},
		"upgrade-insecure-requests": {"1"},
		"user-agent":                {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {
			"accept", "accept-language", "sec-ch-ua", "sec-ch-ua-mobile",
			"sec-ch-ua-platform", "sec-fetch-dest", "sec-fetch-mode",
			"sec-fetch-site", "sec-fetch-user", "upgrade-insecure-requests", "user-agent",
		},
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	return resp.StatusCode, body, resp.Header.Get("Location"), nil
}

func isCaptcha(body []byte) bool {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "@marketfront/") || strings.Contains(s, `data-baobab-name="$page"`) {
		return false
	}
	return strings.Contains(s, "smartcaptcha") || strings.Contains(s, "showcaptcha") ||
		strings.Contains(s, "checkbox-captcha")
}

func first(re *regexp.Regexp, body []byte) string {
	if m := re.FindSubmatch(body); len(m) > 1 {
		return strings.TrimSpace(string(m[1]))
	}
	return ""
}

// firstCardLink ищет в теле ссылку живой формы /card/<slug>/<id> для этого id —
// со слагом, отличным от нашей заглушки.
func firstCardLink(body []byte, id string) string {
	re := regexp.MustCompile(`/card/[a-zA-Z0-9а-яА-Я\-_%]+/` + regexp.QuoteMeta(id))
	for _, m := range re.FindAllString(string(body), 20) {
		if !strings.HasPrefix(m, "/card/x/") && !strings.HasPrefix(m, "/card/-/") {
			return "https://market.yandex.ru" + m
		}
	}
	return ""
}

func firstNonEmpty(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

func trim(s string, n int) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > n {
		return s[:n]
	}
	return s
}
