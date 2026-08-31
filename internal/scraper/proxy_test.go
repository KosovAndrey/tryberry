package scraper

import (
	"net/http"
	"testing"
	"time"
)

// Разброс по плечам работает только на НОВЫХ соединениях: xray выбирает плечо
// на соединение, поэтому у поискового пула keep-alive должен выключаться.
func TestProxyPoolDisableKeepAlives(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts ProxyPoolOptions
		want bool
	}{
		{"по умолчанию keep-alive живой", ProxyPoolOptions{}, false},
		{"выключенный keep-alive", ProxyPoolOptions{DisableKeepAlives: true}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool, errs := NewProxyPoolWithOptions([]string{"http://xray:8889"}, 12*time.Second, tc.opts)
			if len(errs) != 0 {
				t.Fatalf("неожиданные ошибки: %v", errs)
			}
			if pool.Size() != 1 {
				t.Fatalf("ожидали один клиент, получили %d", pool.Size())
			}
			tr, ok := pool.clients[0].client.Transport.(*http.Transport)
			if !ok {
				t.Fatalf("транспорт не *http.Transport: %T", pool.clients[0].client.Transport)
			}
			if tr.DisableKeepAlives != tc.want {
				t.Fatalf("DisableKeepAlives=%v, ожидали %v", tr.DisableKeepAlives, tc.want)
			}
		})
	}
}
