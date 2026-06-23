package telegram

import "testing"

func TestSafeButtonURL(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"clean", "https://www.wildberries.ru/catalog/123/detail.aspx", "https://www.wildberries.ru/catalog/123/detail.aspx"},
		{"name+newline+url (WB share)", "Huawei ремешок, 18 мм\nhttps://www.wildberries.ru/catalog/794032172/detail.aspx?size=1", "https://www.wildberries.ru/catalog/794032172/detail.aspx?size=1"},
		{"command prefix", "/track https://www.ozon.ru/product/abc-3105022586/?at=x", "https://www.ozon.ru/product/abc-3105022586/?at=x"},
		{"leading/trailing space", "  https://market.yandex.ru/card/x/123  ", "https://market.yandex.ru/card/x/123"},
		{"empty", "", ""},
		{"plain text no url", "просто текст без ссылки", ""},
		{"non-http scheme", "ftp://example.com/file", ""},
		{"scheme but no host", "https://", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := safeButtonURL(tt.in); got != tt.want {
				t.Errorf("safeButtonURL(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
