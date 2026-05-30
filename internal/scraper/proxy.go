package scraper

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"time"
)

// proxyClient связывает прокси (метка для логов) с готовым http.Client.
type proxyClient struct {
	label  string // host прокси либо "direct"
	client *http.Client
}

// ProxyPool — пул HTTP-клиентов с ротацией по кругу (round-robin).
//
// Каждый прокси получает собственный *http.Client (переиспользование
// keep-alive соединений на прокси). Пустой список → один прямой клиент,
// то есть код работает и без прокси (просто чаще ловит 429).
//
// Зачем пул: WB режет search.wb.ru по частоте на один IP (эмпирически ~2
// быстрых запроса, затем 429). Несколько ротируемых прокси раскидывают
// запросы по разным IP, и лимит «по кругу» не накапливается.
type ProxyPool struct {
	clients []proxyClient
	idx     uint64 // атомарный счётчик round-robin
}

// NewProxyPool строит пул из списка прокси-URL вида
// "http://user:pass@host:port" или "socks5://host:port".
//
// Невалидные записи пропускаются и возвращаются отдельным срезом ошибок —
// один битый прокси в .env не должен ронять весь скрейпер.
func NewProxyPool(proxyURLs []string, timeout time.Duration) (*ProxyPool, []error) {
	if timeout <= 0 {
		timeout = 12 * time.Second
	}

	var errs []error
	var clients []proxyClient

	for _, raw := range proxyURLs {
		raw = strings.TrimSpace(raw)
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" {
			errs = append(errs, fmt.Errorf("proxy %q: invalid url", raw))
			continue
		}
		tr := &http.Transport{
			Proxy:               http.ProxyURL(u),
			MaxIdleConns:        10,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
		}
		clients = append(clients, proxyClient{
			label:  u.Host,
			client: &http.Client{Timeout: timeout, Transport: tr},
		})
	}

	if len(clients) == 0 {
		// Прямой клиент как единственный — пул всегда непустой.
		clients = append(clients, proxyClient{
			label:  "direct",
			client: &http.Client{Timeout: timeout},
		})
	}

	return &ProxyPool{clients: clients}, errs
}

// next возвращает следующий клиент по кругу (потокобезопасно).
func (p *ProxyPool) next() proxyClient {
	i := atomic.AddUint64(&p.idx, 1) - 1
	return p.clients[int(i%uint64(len(p.clients)))]
}

// Size — число клиентов в пуле (>=1).
func (p *ProxyPool) Size() int { return len(p.clients) }

// HasProxies — true если есть хотя бы один реальный прокси (не direct).
func (p *ProxyPool) HasProxies() bool {
	return len(p.clients) > 1 || (len(p.clients) == 1 && p.clients[0].label != "direct")
}
