// Command ym-card-probe — замер: какая доля наших id Я.Маркета открывается живой
// формой /card/. НЕ часть прод-флоу.
//
// Контекст (01-09-2026). Яндекс закрыл SmartCaptcha обе формы, которыми мы
// пользуемся: и /product--<slug>/<id> (её строит парсер выдачи), и /product/<id>
// (наш канон, миграции 030/031). Обе отдают 302 на showcaptcha даже с живого
// RU-адреса и с прогретыми куками — проверено yandex-probe, все четыре сценария
// blocked. Открывается только /card/<slug>/<id>, причём слаг декоративен:
// /card/x/5193397317 отдал полную карточку 2221 КБ и сам назвал канонический
// адрес в теле. А /card/x/923133126 (наш modelId) отдал скелет 1367 КБ — то есть
// не всякий наш id живёт в пространстве /card/.
//
// Отсюда вопрос, от которого зависит объём починки: сколько из 14086 наших id
// открываются как /card/. Пробник берёт id (аргументы или stdin по одному в
// строке), делает ОДИН GET на /card/x/<id> и классифицирует ответ:
//
//	card     — полная карточка (в теле нашёлся канонический /card/<slug>/<id>)
//	skeleton — 200, но карточки нет: id в пространстве /card/ не существует
//	captcha  — антибот (значит выход сгорел, замер недостоверен)
//
// Трафик щадим: карточка весит ~2,2 МБ, поэтому боевым скрейпером цену проверяем
// только у первых -scrape штук, остальным хватает классификации по телу.
//
// Запуск:
//
//	docker build --build-arg SERVICE=ym-card-probe -t ym-card-probe .
//	psql ... -At -c "select url from products where ..." | sed 's#.*/##' > ids.txt
//	docker run --rm -i -e YM_PROXY="http://user:pass@ip:port" ym-card-probe < ids.txt
package main

import (
	"bufio"
	"context"
	"flag"
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

func main() {
	scrapeN := flag.Int("scrape", 5, "у скольких первых живых карточек проверить цену боевым скрейпером")
	rps := flag.Float64("rps", 1, "запросов в секунду (щадим прокси)")
	flag.Parse()

	proxy := os.Getenv("YM_PROXY")
	if proxy == "" {
		fmt.Fprintln(os.Stderr, "FATAL: задай YM_PROXY, например http://user:pass@ip:port")
		os.Exit(2)
	}

	ids := flag.Args()
	if len(ids) == 0 {
		sc := bufio.NewScanner(os.Stdin)
		for sc.Scan() {
			if v := strings.TrimSpace(sc.Text()); v != "" {
				ids = append(ids, v)
			}
		}
	}
	if len(ids) == 0 {
		fmt.Fprintln(os.Stderr, "FATAL: не передан ни один id (аргументами или в stdin)")
		os.Exit(2)
	}

	client, err := tls_client.NewHttpClient(tls_client.NewNoopLogger(),
		tls_client.WithTimeoutSeconds(40),
		tls_client.WithClientProfile(profiles.Chrome_146),
		tls_client.WithCookieJar(tls_client.NewCookieJar()),
		tls_client.WithProxyUrl(proxy),
	)
	if err != nil {
		fmt.Println("прокси не принят клиентом:", err)
		os.Exit(1)
	}
	live := scraper.NewYandexMarketScraper(scraper.YandexMarketOptions{
		ProxyURL: proxy, ProxyPrimary: true, RPS: *rps,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})

	var card, skeleton, captcha, failed, priced, priceFail int
	pause := time.Duration(float64(time.Second) / *rps)

	fmt.Printf("%-14s %-9s %8s  %s\n", "id", "вердикт", "КБ", "канонический адрес / цена")
	fmt.Println(strings.Repeat("─", 100))

	for i, id := range ids {
		if i > 0 {
			time.Sleep(pause)
		}
		body, err := get(client, "https://market.yandex.ru/card/x/"+id)
		if err != nil {
			failed++
			fmt.Printf("%-14s %-9s %8s  %v\n", id, "ошибка", "—", err)
			continue
		}
		kb := fmt.Sprintf("%d", len(body)/1024)
		switch {
		case isCaptcha(body):
			captcha++
			fmt.Printf("%-14s %-9s %8s\n", id, "КАПЧА", kb)
		default:
			canon := cardLink(body, id)
			if canon == "" {
				skeleton++
				fmt.Printf("%-14s %-9s %8s\n", id, "скелет", kb)
				continue
			}
			card++
			line := fmt.Sprintf("%-14s %-9s %8s  %s", id, "карточка", kb, trim(canon, 60))
			if priced+priceFail < *scrapeN {
				ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
				res, serr := live.Scrape(ctx, canon)
				cancel()
				if serr != nil {
					priceFail++
					line += fmt.Sprintf("  ❌ %v", serr)
				} else {
					priced++
					line += fmt.Sprintf("  ✅ %.2f₽ в наличии=%v", res.Price, res.InStock)
				}
			}
			fmt.Println(line)
		}
	}

	total := len(ids)
	fmt.Println(strings.Repeat("─", 100))
	fmt.Printf("итог по %d id: карточка %d (%.0f%%), скелет %d (%.0f%%), капча %d, ошибка %d\n",
		total, card, pct(card, total), skeleton, pct(skeleton, total), captcha, failed)
	if priced+priceFail > 0 {
		fmt.Printf("цена боевым скрейпером: %d из %d\n", priced, priced+priceFail)
	}
	if captcha > 0 {
		fmt.Println("ВНИМАНИЕ: были капчи — выход подгорел, доля живых id занижена, повторить позже")
	}
}

func pct(n, total int) float64 {
	if total == 0 {
		return 0
	}
	return float64(n) * 100 / float64(total)
}

func get(c tls_client.HttpClient, u string) ([]byte, error) {
	req, err := fhttp.NewRequest("GET", u, nil)
	if err != nil {
		return nil, err
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
		return nil, err
	}
	defer resp.Body.Close()
	return io.ReadAll(io.LimitReader(resp.Body, 6<<20))
}

func isCaptcha(body []byte) bool {
	s := strings.ToLower(string(body))
	if strings.Contains(s, "@marketfront/") || strings.Contains(s, `data-baobab-name="$page"`) {
		return false
	}
	return strings.Contains(s, "smartcaptcha") || strings.Contains(s, "showcaptcha") ||
		strings.Contains(s, "checkbox-captcha")
}

// cardLink — настоящий /card/<slug>/<id> для этого id из тела страницы (её отдаёт
// сама карточка, открытая по фиктивному слагу). Пусто = карточки нет (скелет).
func cardLink(body []byte, id string) string {
	re := regexp.MustCompile(`/card/[a-zA-Z0-9\-_%]+/` + regexp.QuoteMeta(id))
	for _, m := range re.FindAllString(string(body), 50) {
		if !strings.HasPrefix(m, "/card/x/") {
			return "https://market.yandex.ru" + m
		}
	}
	return ""
}

func trim(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}
