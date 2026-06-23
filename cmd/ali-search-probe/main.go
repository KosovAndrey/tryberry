// Command ali-search-probe — discovery поисковой выдачи aliexpress.ru.
//
// Выдача грузится XHR'ом: POST https://aliexpress.ru/aer-webapi/v1/search
// (JSON-API, как карточка /aer-jsonapi/productData). Probe дёргает его через
// ali-транспорт (direct + прогрев aer-cookie через прокси) с МИНИМАЛЬНЫМ телом
// (только searchText/page, без волатильного searchInfo — проверяем, что скрейперу
// хватит запроса) и печатает: status/blocked, самый длинный массив объектов
// (=позиции) и первый элемент целиком — по нему пишем парсер.
//
// Запуск на VPS:
//
//	docker build --build-arg SERVICE=ali-search-probe -t ali-search-probe .
//	docker run --rm --env-file .env ali-search-probe "футболка"
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"

	fhttp "github.com/bogdanfinn/fhttp"
	tls_client "github.com/bogdanfinn/tls-client"
	"github.com/bogdanfinn/tls-client/profiles"
)

const (
	aliBase   = "https://aliexpress.ru"
	searchAPI = aliBase + "/aer-webapi/v1/search"
	userAgent = "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/146.0.0.0 Safari/537.36"
	bodyCap   = 8 << 20
)

func main() {
	proxyURL := os.Getenv("ALI_PROXY_URL")
	if proxyURL == "" {
		proxyURL = os.Getenv("OZON_PROXY_URL")
	}
	if proxyURL == "" {
		fmt.Fprintln(os.Stderr, "FATAL: ALI_PROXY_URL/OZON_PROXY_URL не задан")
		os.Exit(2)
	}
	query := "футболка"
	if len(os.Args) > 1 {
		query = os.Args[1]
	}

	jar := tls_client.NewCookieJar()
	direct := mkClient("", jar)
	proxy := mkClient(proxyURL, jar)
	seedLocale(direct, jar)

	ctx := context.Background()
	fmt.Printf("query: %q\n", query)

	// Прогрев: GET страницы выдачи через прокси — проходит X5SEC и кладёт aer-cookie
	// в общий jar. Затем POST к API (direct, fallback proxy).
	warmURL := aliBase + "/wholesale?SearchText=" + url.QueryEscape(query)
	ws, wb := do(ctx, proxy, fhttp.MethodGet, warmURL, nil, warmURL)
	fmt.Printf("warmup(proxy): status=%d blocked=%s len=%d cookies=%d\n", ws, yn(isBlocked(wb)), len(wb), cookieCount(proxy))
	time.Sleep(1200 * time.Millisecond)

	body := []byte(fmt.Sprintf(`{"page":1,"searchText":%q,"source":"direct","catId":"","storeIds":[],"pgChildren":[],"aeBrainIds":[],"mainFilters":"","searchTrigger":"search_bar","g":"y"}`, query))
	status, resp := do(ctx, direct, fhttp.MethodPost, searchAPI, body, warmURL)
	src := "direct"
	if status != 200 || isBlocked(resp) {
		status, resp = do(ctx, proxy, fhttp.MethodPost, searchAPI, body, warmURL)
		src = "proxy"
	}
	fmt.Printf("search POST(%s): status=%d blocked=%s len=%d\n\n", src, status, yn(isBlocked(resp)), len(resp))

	report(resp)
}

func report(body []byte) {
	var root interface{}
	if err := json.Unmarshal(body, &root); err != nil {
		fmt.Printf("ответ не JSON: %v\n  начало: %s\n", err, snippet(body, 300))
		return
	}
	if m, ok := root.(map[string]interface{}); ok {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		fmt.Printf("top-level keys: %s\n", strings.Join(keys, ", "))
	}

	// Товары лежат в data.productsFeed.productsV2[] — печатаем первый ЦЕЛИКОМ.
	if items := dig(root, "data", "productsFeed", "productsV2"); items != nil {
		if arr, ok := items.([]interface{}); ok && len(arr) > 0 {
			fmt.Printf("productsFeed.productsV2: %d позиций\n\nПЕРВАЯ ПОЗИЦИЯ ЦЕЛИКОМ:\n", len(arr))
			pretty, _ := json.MarshalIndent(arr[0], "", "  ")
			fmt.Println(string(pretty))
			return
		}
	}
	fmt.Printf("productsV2 не найден; начало ответа:\n%s\n", snippet(body, 1500))
}

// dig спускается по ключам объектов: dig(root,"data","productsFeed","productsV2").
func dig(v interface{}, keys ...string) interface{} {
	for _, k := range keys {
		m, ok := v.(map[string]interface{})
		if !ok {
			return nil
		}
		v = m[k]
	}
	return v
}

func do(ctx context.Context, client tls_client.HttpClient, method, target string, body []byte, referer string) (int, []byte) {
	var rdr io.Reader
	if body != nil {
		rdr = strings.NewReader(string(body))
	}
	req, err := fhttp.NewRequestWithContext(ctx, method, target, rdr)
	if err != nil {
		return 0, nil
	}
	h := fhttp.Header{
		"accept":          {"*/*"},
		"accept-language": {"ru,en;q=0.9"},
		"bx-v":            {"2.5.36"},
		"origin":          {aliBase},
		"referer":         {referer},
		"user-agent":      {userAgent},
	}
	if method == fhttp.MethodPost {
		h["content-type"] = []string{"application/json"}
		h["x-requested-with"] = []string{"XMLHttpRequest"}
	}
	req.Header = h
	resp, err := client.Do(req)
	if err != nil {
		fmt.Fprintln(os.Stderr, "request:", err)
		return 0, nil
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, bodyCap))
	return resp.StatusCode, b
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

func seedLocale(client tls_client.HttpClient, jar tls_client.CookieJar) {
	u, _ := url.Parse(aliBase)
	client.SetCookies(u, []*fhttp.Cookie{{
		Name: "aep_usuc_f", Value: "site=rus&c_tp=RUB&region=RU&b_locale=ru_RU",
		Domain: ".aliexpress.ru", Path: "/",
	}})
}

func cookieCount(client tls_client.HttpClient) int {
	u, _ := url.Parse(aliBase)
	return len(client.GetCookies(u))
}

func isBlocked(body []byte) bool {
	s := string(body)
	return strings.Contains(s, "x5secdata") || strings.Contains(s, "_____tmd_____") ||
		strings.Contains(s, "punish") || strings.Contains(s, "Slider captcha")
}

func snippet(b []byte, n int) string {
	if len(b) > n {
		b = b[:n]
	}
	return strings.Join(strings.Fields(string(b)), " ")
}

func yn(b bool) string {
	if b {
		return "YES"
	}
	return "no"
}
