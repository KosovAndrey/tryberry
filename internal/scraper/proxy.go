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
// Зачем пул: WB режет поисковую ручку по частоте на один IP (эмпирически ~2
// быстрых запроса, затем 429). Несколько ротируемых прокси раскидывают
// запросы по разным IP, и лимит «по кругу» не накапливается.
//
// ВАЖНО про keep-alive (разбор 01-09-2026). Когда за ОДНИМ прокси-URL стоит
// балансировщик, выбирающий плечо на СОЕДИНЕНИЕ (у нас xray :8889 со стратегией
// random), keep-alive съедает весь смысл разброса: клиент переиспользует
// MaxIdleConnsPerHost соединений, каждое приклеено к своему плечу, и поток
// ходит с горстки адресов, сколько бы плеч ни было в конфиге. Именно так
// расширение пула с 10 до 24 плеч не изменило долю 429 (52% → 55%).
// Лечится DisableKeepAlives: новое соединение на запрос = новое плечо.
type ProxyPool struct {
	clients []proxyClient
	idx     uint64 // атомарный счётчик round-robin
}

// NewProxyPool строит пул из списка прокси-URL вида
// "http://user:pass@host:port" или "socks5://host:port".
//
// Невалидные записи пропускаются и возвращаются отдельным срезом ошибок —
// один битый прокси в .env не должен ронять весь скрейпер.
// ProxyPoolOptions — поведение клиентов пула.
type ProxyPoolOptions struct {
	// DisableKeepAlives — закрывать соединение после каждого запроса. Нужен,
	// когда за прокси стоит балансировщик «плечо на соединение»: иначе разброс
	// по плечам не работает (см. комментарий к ProxyPool). Цена — TLS-хендшейк
	// на запрос; она заметно меньше, чем ретраи по 429 с backoff до 8с.
	DisableKeepAlives bool
}

func NewProxyPool(proxyURLs []string, timeout time.Duration) (*ProxyPool, []error) {
	return NewProxyPoolWithOptions(proxyURLs, timeout, ProxyPoolOptions{})
}

// NewProxyPoolWithOptions — то же, но с настройками транспорта.
func NewProxyPoolWithOptions(proxyURLs []string, timeout time.Duration,
	opts ProxyPoolOptions) (*ProxyPool, []error) {
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
			DisableKeepAlives:   opts.DisableKeepAlives,
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
