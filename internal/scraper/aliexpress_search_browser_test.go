package scraper

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
)

// roundTripFunc — стаб транспорта к сайдкару: канонические ответы без реальной
// сети (httptest требует loopback TCP, которого в песочнице нет).
type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func mkResp(status int, body string) *http.Response {
	return &http.Response{
		StatusCode: status,
		Body:       io.NopCloser(strings.NewReader(body)),
		Header:     make(http.Header),
	}
}

func TestAliFetchOutcome(t *testing.T) {
	block := `{"data":{},"x5secdata":"..."}`
	empty := `{"data":{"productsFeed":{"productsV2":[]}}}`
	cases := []struct {
		name   string
		status int
		body   string
		err    error
		want   string
	}{
		{"transport error", 0, "", io.ErrUnexpectedEOF, "error"},
		{"x5sec block", 200, block, nil, "blocked"},
		{"non-200", 500, "oops", nil, "other"},
		{"200 no products", 200, empty, nil, "empty"},
		{"200 with products", 200, aliSearchSample, nil, "ok"},
	}
	for _, tt := range cases {
		t.Run(tt.name, func(t *testing.T) {
			if got := aliFetchOutcome(tt.status, []byte(tt.body), tt.err); got != tt.want {
				t.Errorf("aliFetchOutcome = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestAliSetBrowserSidecar(t *testing.T) {
	s := NewAliexpressSearchScraper(&AliexpressScraper{}, 60)

	// Пустой URL — фолбэка нет (клиент не создаётся), но browserMaxPages ставится.
	s.SetBrowserSidecar("", 0)
	if s.browserURL != "" || s.browserClient != nil {
		t.Errorf("empty URL must leave sidecar off, got url=%q client=%v", s.browserURL, s.browserClient)
	}
	if s.browserMaxPages != 1 {
		t.Errorf("maxPages<=0 must default to 1, got %d", s.browserMaxPages)
	}

	// Заданный URL с trailing slash — нормализуется, клиент создаётся.
	s.SetBrowserSidecar("http://ali-miner:8082/", 3)
	if s.browserURL != "http://ali-miner:8082" {
		t.Errorf("browserURL = %q, want trimmed", s.browserURL)
	}
	if s.browserClient == nil {
		t.Error("browserClient must be set for non-empty URL")
	}
	if s.browserMaxPages != 3 {
		t.Errorf("browserMaxPages = %d, want 3", s.browserMaxPages)
	}
}

func TestAliFetchViaBrowser(t *testing.T) {
	s := NewAliexpressSearchScraper(&AliexpressScraper{}, 60)

	// Не сконфигурён → ошибка.
	if _, _, err := s.fetchViaBrowser(context.Background(), "телефон", 1); err == nil {
		t.Error("expected error when sidecar not configured")
	}

	// Стаб: проверяем, что запрос уходит на /search с text/page, и тело зеркалится.
	var gotURL string
	s.SetBrowserSidecar("http://ali-miner:8082", 1)
	s.browserClient = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
		gotURL = r.URL.String()
		return mkResp(200, aliSearchSample), nil
	})}

	status, body, err := s.fetchViaBrowser(context.Background(), "телефон", 2)
	if err != nil {
		t.Fatalf("fetchViaBrowser: %v", err)
	}
	if status != 200 {
		t.Errorf("status = %d, want 200", status)
	}
	if !strings.Contains(string(body), "snippetContainer") {
		t.Errorf("body not mirrored: %q", string(body))
	}
	if !strings.Contains(gotURL, "/search?") ||
		!strings.Contains(gotURL, "page=2") ||
		!strings.Contains(gotURL, "text=") {
		t.Errorf("unexpected sidecar URL: %q", gotURL)
	}
}
