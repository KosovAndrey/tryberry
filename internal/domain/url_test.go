package domain

import "testing"

func TestCleanProductURL(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{
			name: "yandex market — дропаем cpc/do-waremd5/ogV",
			in:   "https://market.yandex.ru/card/noski-vysokiye/5130704659?do-waremd5=mpuRPKss1ZAK&cpc=k2uRTiyt00SsrEruJLXvzDx3&ogV=-12",
			want: "https://market.yandex.ru/card/noski-vysokiye/5130704659",
		},
		{
			name: "ozon — дропаем from/perehod/__rr",
			in:   "https://www.ozon.ru/product/zubnaya-shchetka-135499001/?from=share_ios&perehod=smm_share_button&__rr=6",
			want: "https://www.ozon.ru/product/zubnaya-shchetka-135499001/",
		},
		{
			name: "wildberries — без query не меняется",
			in:   "https://www.wildberries.ru/catalog/12345678/detail.aspx",
			want: "https://www.wildberries.ru/catalog/12345678/detail.aspx",
		},
		{
			name: "aliexpress — дропаем query и fragment",
			in:   "https://aliexpress.ru/item/1005006.html?sku_id=12000&spm=a2g2w#reviews",
			want: "https://aliexpress.ru/item/1005006.html",
		},
		{
			name: "пустая строка",
			in:   "",
			want: "",
		},
		{
			name: "не-URL возвращаем как есть",
			in:   "не ссылка",
			want: "не ссылка",
		},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := CleanProductURL(c.in); got != c.want {
				t.Errorf("CleanProductURL(%q) = %q, want %q", c.in, got, c.want)
			}
		})
	}
}
