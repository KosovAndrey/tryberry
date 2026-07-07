package scraper

import (
	"net"
	"net/http"
	"time"
)

// newTunedHTTPTransport — переиспользуемый direct-транспорт с расширенным пулом
// keep-alive соединений для скрейперов, ходящих ПЛОСКИМ net/http на маркетплейсы,
// CDN и внутренние сайдкары частыми запросами.
//
// Зачем. http.DefaultTransport (который получает голый &http.Client{}) держит лишь
// 2 idle-соединения на хост (DefaultMaxIdleConnsPerHost). Под конкуррентным
// консьюмером scraper (пул из CONSUMER_CONCURRENCY горутин) это заставляет на каждом
// всплеске пересоздавать TCP+TLS-рукопожатие вместо переиспользования сокета —
// лишняя латентность и нагрузка. Поднимаем per-host лимит до 32 и явно фиксируем
// keep-alive и таймауты рукопожатия/дозвона.
//
// ВАЖНО про антибот. Это ПЛОСКИЙ Go-транспорт, он НЕ меняет TLS-отпечаток (JA3): им
// ходят только «чистые» пути, где антибот по транспорту не проверяет — WB basket-CDN
// и витрина продавца (открытый каталог), внутренние сайдкары (ozon-miner,
// wb-search-miner). Antibot-дорожки с проверкой отпечатка (Ozon FAB, AliExpress X5SEC,
// Я.Маркет SmartCaptcha) ходят через bogdanfinn/tls-client и здесь НЕ участвуют.
//
// Прокси-резолюция сохраняется ровно как у DefaultTransport (ProxyFromEnvironment):
// клиенты, которые раньше получали DefaultTransport, продолжают уважать HTTPS_PROXY/
// NO_PROXY из окружения — меняем только пул и таймауты, не проксирование.
func newTunedHTTPTransport() *http.Transport {
	return &http.Transport{
		// Как у http.DefaultTransport — не меняем логику проксирования, только пул.
		Proxy: http.ProxyFromEnvironment,
		DialContext: (&net.Dialer{
			Timeout:   10 * time.Second, // дозвон; общий потолок всё равно даёт Client.Timeout
			KeepAlive: 30 * time.Second, // TCP keep-alive включён явно
		}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          100,
		MaxIdleConnsPerHost:   32, // маркетплейс/CDN — несколько хостов, по каждому ходим часто
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
	}
}
