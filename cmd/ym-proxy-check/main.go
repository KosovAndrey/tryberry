// Command ym-proxy-check — проверка кандидата в RU-прокси для Я.Маркета ДО оплаты.
//
// Зачем отдельный инструмент: curl для этой проверки не годится (его TLS-отпечаток
// сам вызывает 302+капчу даже там, где боевой клиент проходит), а «страна = RU» ещё
// ничего не значит — 01-09-2026 выяснилось, что Яндекс режет ХОСТИНГОВЫЕ сети, в том
// числе российские (Selectel — капча даже на главной), а адреса домашних провайдеров
// (ЭР-Телеком, Билайн, ГТНТ) проходят. Поэтому проверяем ровно тем транспортом, что
// в проде, и заодно печатаем, чья это сеть.
//
// Запуск:
//
//	docker build --build-arg SERVICE=ym-proxy-check -t ym-proxy-check .
//	docker run --rm -e YM_PROXY="socks5://user:pass@ip:port" ym-proxy-check
//
// Схема прокси: http:// или socks5://. Свои URL можно передать аргументами.
package main

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"

	"gitlab.com/KosovAndrey/tryberrybot/internal/scraper"
)

var defaultURLs = []string{
	"https://market.yandex.ru/card/kofemashina-jura-e8-15584/5193397317",
	"https://market.yandex.ru/card/pogruzhnaya-yaytsevarka/6129566325",
	"https://market.yandex.ru/card/yaytsevarka-kitfort-kt-9802/103725151839",
}

func main() {
	proxy := os.Getenv("YM_PROXY")
	if proxy == "" {
		fmt.Fprintln(os.Stderr, "FATAL: задай YM_PROXY, например socks5://user:pass@ip:port")
		os.Exit(2)
	}
	urls := os.Args[1:]
	if len(urls) == 0 {
		urls = defaultURLs
	}

	raw, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(40),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithProxyUrl(proxy),
	)
	if err != nil {
		fmt.Println("прокси не принят клиентом:", err)
		os.Exit(1)
	}

	// 1. Чей это адрес. Хостинговая сеть — почти наверняка капча, даже если RU.
	if st, body, _, err := get(raw, "https://ipinfo.io/json"); err != nil {
		fmt.Printf("❌ прокси недоступен: %v\n", err)
		os.Exit(1)
	} else {
		fmt.Printf("выход (%d): %s\n\n", st, compact(body, 400))
	}

	// 2. Капча или нет — по заголовку X-Yandex-Captcha на главной и карточке.
	for _, u := range []string{"https://market.yandex.ru/", urls[0]} {
		st, body, captcha, err := get(raw, u)
		switch {
		case err != nil:
			fmt.Printf("❌ %-62s ошибка %v\n", short(u), err)
		case captcha:
			fmt.Printf("⛔ %-62s КАПЧА (статус %d, %d КБ)\n", short(u), st, len(body)/1024)
		default:
			fmt.Printf("✅ %-62s чисто (статус %d, %d КБ)\n", short(u), st, len(body)/1024)
		}
	}

	// 3. Боевой скрейпер: цена реально достаётся или нет.
	fmt.Println("\nбоевой скрейпер через прокси:")
	s := scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
		ProxyURL: proxy, ProxyPrimary: true, RPS: 2,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	ok := 0
	for _, u := range urls {
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		t0 := time.Now()
		res, err := s.Scrape(ctx, u)
		cancel()
		if err != nil {
			fmt.Printf("❌ %-62s %v\n", short(u), err)
			continue
		}
		ok++
		fmt.Printf("✅ %-62s %9.2f₽ в наличии=%-5v %s\n", short(u), res.Price, res.InStock, time.Since(t0).Round(100*time.Millisecond))
	}
	fmt.Printf("\nитог: %d из %d карточек с ценой. ", ok, len(urls))
	if ok == len(urls) {
		fmt.Println("Прокси годится — можно вписывать в YANDEX_PROXY_URL.")
		return
	}
	fmt.Println("Прокси НЕ годится: нужен адрес домашнего провайдера или мобильный, не хостинговый.")
	os.Exit(1)
}

func get(c tls_client.HttpClient, u string) (int, []byte, bool, error) {
	req, _ := fhttp.NewRequest("GET", u, nil)
	req.Header = fhttp.Header{
		"accept":             {"text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8"},
		"accept-language":    {"ru,en;q=0.9"},
		"user-agent":         {"Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36"},
		fhttp.HeaderOrderKey: {"accept", "accept-language", "user-agent"},
	}
	resp, err := c.Do(req)
	if err != nil {
		return 0, nil, false, err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	return resp.StatusCode, b, resp.Header.Get("X-Yandex-Captcha") != "", nil
}

func compact(b []byte, max int) string {
	s := string(b)
	if len(s) > max {
		s = s[:max]
	}
	out := make([]rune, 0, len(s))
	space := false
	for _, r := range s {
		if r == '\n' || r == '\t' || r == ' ' {
			if !space {
				out = append(out, ' ')
				space = true
			}
			continue
		}
		space = false
		out = append(out, r)
	}
	return string(out)
}

func short(u string) string {
	if len(u) > 60 {
		return u[:60]
	}
	return u
}
