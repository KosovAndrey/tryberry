package scraper

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestExtractArticleID(t *testing.T) {
	tests := []struct {
		url     string
		want    string
		wantErr bool
	}{
		{
			url:  "https://www.wildberries.ru/catalog/506268209/detail.aspx",
			want: "506268209",
		},
		{
			url:  "https://www.wildberries.ru/catalog/205494044/detail.aspx?size=330251041",
			want: "205494044",
		},
		{
			url:     "https://example.com/not-a-wb-url",
			wantErr: true,
		},
	}

	for _, tc := range tests {
		got, err := ExtractArticleID(tc.url)
		if tc.wantErr {
			if err == nil {
				t.Errorf("url=%q: ожидали ошибку, получили nil", tc.url)
			}
			continue
		}
		if err != nil {
			t.Errorf("url=%q: неожиданная ошибка: %v", tc.url, err)
			continue
		}
		if got != tc.want {
			t.Errorf("url=%q: got %q, want %q", tc.url, got, tc.want)
		}
	}
}

func TestClient_Scrape(t *testing.T) {
	// Мок-сервер возвращает валидный ответ WB API
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := wbResponse{}
		resp.Data.Products = []struct {
			Name  string `json:"name"`
			Sizes []struct {
				Price struct {
					Total int64 `json:"total"`
				} `json:"price"`
			} `json:"sizes"`
			Photos []struct {
				Big string `json:"big"`
			} `json:"photos"`
		}{
			{
				Name: "Тестовый товар",
				Sizes: []struct {
					Price struct {
						Total int64 `json:"total"`
					} `json:"price"`
				}{
					{Price: struct {
						Total int64 `json:"total"`
					}{Total: 150000}}, // 1500 рублей
				},
				Photos: []struct {
					Big string `json:"big"`
				}{
					{Big: "https://images.wbstatic.net/test.jpg"},
				},
			},
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(resp)
	}))
	defer srv.Close()

	// Подменяем http-клиент чтобы запросы шли на мок-сервер
	c := NewClient(10)
	c.http = &http.Client{
		Transport: &mockTransport{target: srv.URL},
	}

	result, err := c.Scrape(context.Background(), "123456789")
	if err != nil {
		t.Fatalf("Scrape вернул ошибку: %v", err)
	}
	if result.Name != "Тестовый товар" {
		t.Errorf("Name: got %q, want %q", result.Name, "Тестовый товар")
	}
	if result.Price != 1500.00 {
		t.Errorf("Price: got %v, want 1500.00", result.Price)
	}
	if result.ImageURL != "https://images.wbstatic.net/test.jpg" {
		t.Errorf("ImageURL: got %q", result.ImageURL)
	}
}

func TestClient_Scrape_EmptyProducts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"data":{"products":[]}}`))
	}))
	defer srv.Close()

	c := NewClient(10)
	c.http = &http.Client{Transport: &mockTransport{target: srv.URL}}

	_, err := c.Scrape(context.Background(), "000")
	if err == nil {
		t.Fatal("ожидали ошибку для пустого products, получили nil")
	}
}

// mockTransport перенаправляет все запросы на тестовый сервер
type mockTransport struct {
	target string
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	req.URL.Host = req.URL.Host // сохраняем query string
	req2, _ := http.NewRequestWithContext(req.Context(), req.Method, m.target, nil)
	req2.URL.RawQuery = req.URL.RawQuery
	return http.DefaultTransport.RoundTrip(req2)
}
