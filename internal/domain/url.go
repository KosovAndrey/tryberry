package domain

import (
	"net/url"
	"strings"
)

// CleanProductURL убирает трекинг-хвост (query + fragment) из ссылки на КАРТОЧКУ
// товара, оставляя scheme+host+path. Этого достаточно, чтобы открыть карточку на
// всех поддерживаемых маркетплейсах (WB/Ozon/Я.Маркет/AliExpress) — товар
// идентифицируется по пути, — а длинные share/трекинг-параметры (do-waremd5, cpc,
// ogV у Я.Маркета; from, perehod, __rr у Ozon и т.п.) выкидываются, чтобы ссылка
// в уведомлении не превращалась в простыню.
//
// ВАЖНО: применять только к ссылкам на карточку. К ссылкам на ВЫДАЧУ (поиск)
// применять нельзя — там query (?text=, ?search=) несёт сам запрос.
//
// На пустом/нераспознаваемом вводе возвращает исходную строку без изменений.
func CleanProductURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return raw
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme == "" || u.Host == "" {
		return raw
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
