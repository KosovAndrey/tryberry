// ym-dump — проба Я.Маркета для диагностики дрейфа вёрстки: качает страницу
// тем же транспортом, что боевой скрейпер (tls-client, Chrome-профиль — иначе
// SmartCaptcha), и пишет сырой HTML в файл. Нужна, когда scraper начинает
// сыпать parse_error и требуется живой дамп страницы под фикс парсера:
//
//	go run ./cmd/ym-dump -url 'https://market.yandex.ru/product--…/123' -out page.html
package main

import (
	"flag"
	"fmt"
	"io"
	"os"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

func main() {
	urlFlag := flag.String("url", "", "URL карточки Я.Маркета")
	out := flag.String("out", "page.html", "куда писать HTML")
	flag.Parse()
	if *urlFlag == "" {
		fmt.Fprintln(os.Stderr, "usage: ym-dump -url <карточка> [-out page.html]")
		os.Exit(2)
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(25),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
	)
	if err != nil {
		fmt.Fprintln(os.Stderr, "tls-client:", err)
		os.Exit(1)
	}

	req, err := fhttp.NewRequest(fhttp.MethodGet, *urlFlag, nil)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		os.Exit(1)
	}
	// Тот же набор заголовков, что ymCardHeader боевого скрейпера.
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

	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "get:", err)
		os.Exit(1)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 6<<20))
	if err := os.WriteFile(*out, body, 0o644); err != nil {
		fmt.Fprintln(os.Stderr, "write:", err)
		os.Exit(1)
	}
	fmt.Printf("status=%d len=%d final_url=%s out=%s\n",
		resp.StatusCode, len(body), resp.Request.URL, *out)
}
