package scraper

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestIsShortLink(t *testing.T) {
	cases := []struct {
		url  string
		want bool
	}{
		{"https://ozon.ru/t/abc123", true},
		{"https://www.ozon.ru/t/abc123", true},
		{"https://m.ozon.ru/t/abc123", true},
		{"https://ozon.ru/product/nazvanie-123456/", false}, // полный URL — не шортнер
		{"https://a.aliexpress.com/_msAbCd", true},
		{"https://s.click.aliexpress.com/e/_xyz", true},
		{"https://aliexpress.ru/e/_abc", true},
		{"https://aliexpress.ru/item/123.html", false}, // полный URL
		{"https://www.wildberries.ru/catalog/123/detail.aspx", false},
		{"https://market.yandex.ru/product--x/123", false},
		{"not a url", false},
		{"", false},
	}
	for _, c := range cases {
		if got := isShortLink(c.url); got != c.want {
			t.Errorf("isShortLink(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

// fakeRT — in-memory RoundTripper: гоняет редирект-цепочку без реальных сокетов
// (loopback в CI/песочнице недоступен). http.Client сам следует за 3xx, дёргая
// RoundTrip на каждый хоп; неизвестный URL отдаёт 200 (конец цепочки).
type fakeRT struct {
	hops map[string]string // url -> Location (302); отсутствует => 200 OK
	err  error             // если задан — RoundTrip всегда падает (имитация сети)
}

func (f fakeRT) RoundTrip(req *http.Request) (*http.Response, error) {
	if f.err != nil {
		return nil, f.err
	}
	h := http.Header{}
	status := http.StatusOK
	if loc, ok := f.hops[req.URL.String()]; ok {
		h.Set("Location", loc)
		status = http.StatusFound
	}
	return &http.Response{
		StatusCode: status,
		Header:     h,
		Body:       io.NopCloser(strings.NewReader("")),
		Request:    req,
	}, nil
}

// withTestShortHost расширяет allowlist хостом для теста и откатывает после.
func withTestShortHost(t *testing.T, host, path string) {
	t.Helper()
	old := shortLinkPatterns
	shortLinkPatterns = append([]shortLinkPattern{{host: host, path: path}}, old...)
	t.Cleanup(func() { shortLinkPatterns = old })
}

func TestExpand_FollowsRedirectChainToCanonical(t *testing.T) {
	withTestShortHost(t, "sh.test", "/t/")
	r := NewLinkResolver(0)
	r.client.Transport = fakeRT{hops: map[string]string{
		"https://sh.test/t/abc": "https://hop.test/r",
		"https://hop.test/r":    "https://aliexpress.ru/item/123.html?from=app",
		// aliexpress.ru/item — конечный товарный URL (нет в hops => 200).
	}}
	got := r.Expand(context.Background(), "https://sh.test/t/abc")
	want := "https://aliexpress.ru/item/123.html?from=app"
	if got != want {
		t.Fatalf("Expand = %q, want %q", got, want)
	}
}

func TestExpand_NonShortLinkUntouchedNoNetwork(t *testing.T) {
	r := NewLinkResolver(0)
	// Транспорт, который падает при любом вызове — доказывает, что для не-шортнера
	// сеть не трогается вообще.
	r.client.Transport = fakeRT{err: errors.New("network must not be called")}
	in := "https://www.wildberries.ru/catalog/123/detail.aspx"
	if got := r.Expand(context.Background(), in); got != in {
		t.Fatalf("Expand mutated/networked non-short link: %q", got)
	}
}

func TestExpand_NetworkErrorKeepsOriginal(t *testing.T) {
	withTestShortHost(t, "sh.test", "/t/")
	r := NewLinkResolver(0)
	r.client.Transport = fakeRT{err: errors.New("boom")}
	in := "https://sh.test/t/dead"
	if got := r.Expand(context.Background(), in); got != in {
		t.Fatalf("on network error Expand = %q, want original %q", got, in)
	}
}

func TestExpandInText_ReplacesOnlyShortLinks(t *testing.T) {
	withTestShortHost(t, "sh.test", "/t/")
	r := NewLinkResolver(0)
	r.client.Transport = fakeRT{hops: map[string]string{
		"https://sh.test/t/x": "https://ozon.ru/product/name-777/",
	}}
	text := "глянь https://sh.test/t/x плиз"
	out := r.ExpandInText(context.Background(), text)
	if !strings.Contains(out, "https://ozon.ru/product/name-777/") {
		t.Fatalf("ExpandInText did not expand short link: %q", out)
	}
	if !strings.HasPrefix(out, "глянь ") || !strings.HasSuffix(out, " плиз") {
		t.Fatalf("ExpandInText lost surrounding text: %q", out)
	}
}

func TestExpandInText_NoURLNoChange(t *testing.T) {
	r := NewLinkResolver(0)
	r.client.Transport = fakeRT{err: errors.New("network must not be called")}
	in := "просто текст без ссылок"
	if got := r.ExpandInText(context.Background(), in); got != in {
		t.Fatalf("ExpandInText changed plain text: %q", got)
	}
}
