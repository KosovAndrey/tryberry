package domain

import (
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// Тексты и парсинг пользовательского ввода, общие для TG- и VK-ботов.
// Всё plain text (без HTML) — TG-обработчики добавляют разметку сами.

// PlanShowcase — порядок и слоганы тарифов на витрине (TG /plans и VK
// «Тарифы»). Цифры (цены/лимиты/интервалы) — в Plans.
var PlanShowcase = []struct {
	Name    string
	Tagline string
}{
	{"lite", "следить за своими покупками"},
	{"pro", "большие списки и быстрые проверки"},
	{"reseller_start", "для перекупов: проверка раз в минуту"},
	{"reseller_pro", "максимум скорости и объёма"},
}

// IntervalPhrase — «каждую минуту / каждые 15 минут / каждый час».
func IntervalPhrase(d time.Duration) string {
	m := int(d.Minutes())
	switch {
	case m <= 1:
		return "каждую минуту"
	case m == 60:
		return "каждый час"
	default:
		return fmt.Sprintf("каждые %d минут", m)
	}
}

// NotifyChannelTitle — человекочитаемое имя канала уведомлений.
func NotifyChannelTitle(ch string) string {
	switch ch {
	case NotifyTG:
		return "Telegram"
	case NotifyVK:
		return "VK"
	case NotifyBoth:
		return "Telegram + VK"
	default:
		return "Telegram" // auto у привязанных трактуем как TG (зарегался в TG)
	}
}

// TriggerDescription — человекочитаемое описание условия уведомления.
func TriggerDescription(t TriggerType, target *float64, pct *int16) string {
	switch t {
	case TriggerBelowTarget:
		if target != nil {
			return fmt.Sprintf("📉 уведомлю, когда цена опустится ниже %.0f ₽", *target)
		}
		return "📉 уведомлю при достижении целевой цены"
	case TriggerAnyDrop:
		return "🔻 уведомлю при любом снижении цены"
	case TriggerDiscountPct:
		if pct != nil {
			return fmt.Sprintf("％ уведомлю при скидке от %d%%", *pct)
		}
		return "％ уведомлю при заметной скидке"
	case TriggerBackInStock:
		return "🔔 уведомлю, когда товар снова появится в наличии"
	default:
		return ""
	}
}

// DaysWord — «3 дня», «7 дней», «21 день».
func DaysWord(n int) string {
	n10, n100 := n%10, n%100
	switch {
	case n10 == 1 && n100 != 11:
		return "день"
	case n10 >= 2 && n10 <= 4 && (n100 < 12 || n100 > 14):
		return "дня"
	default:
		return "дней"
	}
}

// QueryTextFromNormalized — человекочитаемый запрос из нормализованного
// поискового URL (...search.aspx?search=...&sort=...) для отображения.
func QueryTextFromNormalized(normalized string) string {
	if u, err := url.Parse(normalized); err == nil {
		// Витрина продавца WB (/seller/{id}): нет текста запроса — собираем ярлык
		// «Магазин #{id}» (+ клиентский текст-фильтр tb_q, если задан).
		if m := sellerPathRe.FindStringSubmatch(u.Path); len(m) == 2 {
			label := "Магазин #" + m[1]
			if tq := strings.TrimSpace(u.Query().Get("tb_q")); tq != "" {
				label += " · " + tq
			}
			return label
		}
		// Витрина продавца Я.Маркета (/business--<slug>/{id}) — ярлык «Магазин #id».
		if m := ymBusinessPathRe.FindStringSubmatch(u.Path); len(m) == 2 {
			return "Магазин #" + m[1]
		}
		// Текст запроса лежит в query-параметре: у WB это search=, у Я.Маркета text=.
		// Берём первый непустой — не зависим от формата маркетплейса и порядка
		// параметров.
		q := u.Query()
		for _, key := range []string{"search", "text", "SearchText"} {
			if v := strings.TrimSpace(q.Get(key)); v != "" {
				return v
			}
		}
	}
	return normalized
}

// sellerPathRe — путь витрины продавца WB /seller/{id} (для ярлыка запроса).
var sellerPathRe = regexp.MustCompile(`/seller/(\d+)`)

// ymBusinessPathRe — путь витрины продавца Я.Маркета /business--<slug>/{id}.
var ymBusinessPathRe = regexp.MustCompile(`/business--[^/]+/(\d+)`)

// sellerVanityRe — ссылка на витрину продавца WB с произвольным слагом.
var sellerVanityRe = regexp.MustCompile(`wildberries\.ru/seller/([^/?#\s]+)`)

// SellerVanitySlug — извлечь БУКВЕННЫЙ слаг витрины из ссылки
// (/seller/moderndevice → "moderndevice", ok). Числовой /seller/123 → ok=false
// (его обрабатывает обычный флоу), как и не-seller-ссылки.
func SellerVanitySlug(s string) (string, bool) {
	m := sellerVanityRe.FindStringSubmatch(s)
	if m == nil {
		return "", false
	}
	for _, r := range m[1] {
		if r < '0' || r > '9' {
			return m[1], true
		}
	}
	return "", false
}

// IsSellerVanityURL — ссылка на витрину продавца с буквенным слагом.
func IsSellerVanityURL(s string) bool {
	_, ok := SellerVanitySlug(s)
	return ok
}

// RewriteSellerVanity — заменить буквенный слаг в /seller/{slug} на числовой id,
// сохранив остальную часть ссылки (схему, www, query с фильтрами).
func RewriteSellerVanity(s, id string) string {
	return sellerVanityRe.ReplaceAllString(s, "wildberries.ru/seller/"+id)
}

// tbTextFilterParam — клиентский текст-фильтр в ссылке витрины продавца. Должен
// совпадать с scraper.tbTextParam (там он применяется к выдаче).
const tbTextFilterParam = "tb_q"

// SellerLabel — человекочитаемый ярлык витрины продавца: имя магазина (+ текст-
// фильтр tb_q, если задан). Пустое имя → «Магазин». Используется как QueryText
// подписки (показывается в списке и уведомлениях).
func SellerLabel(name, rawURL string) string {
	label := strings.TrimSpace(name)
	if label == "" {
		label = "Магазин"
	}
	if u, err := url.Parse(rawURL); err == nil {
		if tq := strings.TrimSpace(u.Query().Get(tbTextFilterParam)); tq != "" {
			label += " · " + tq
		}
	}
	return label
}

// HasTextFilter — есть ли уже клиентский текст-фильтр (tb_q) в ссылке.
func HasTextFilter(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	return strings.TrimSpace(u.Query().Get(tbTextFilterParam)) != ""
}

// searchNonFilterParams — query-параметры поисковой ссылки, которые НЕ сужают
// выдачу (сам запрос, пагинация, сортировка, регион, трекинг). Всё остальное в
// query трактуем как фильтр маркетплейса (категория hid/nid, бренд/цена glfilter,
// WB f<digits>/priceU и т.п.). Список — нижний регистр.
var searchNonFilterParams = map[string]bool{
	"text": true, "search": true, "tb_q": true, // сам запрос / наш текст-фильтр
	"page": true, "sort": true, "sorting": true,
	"lr": true, "clid": true, "rs": true, "rt": true, // регион/трекинг Я.Маркета
	"suggest_text": true, "suggesttext": true, "was_redir": true,
	"from": true, "from_global": true,
	"utm_source": true, "utm_medium": true, "utm_campaign": true,
	"utm_term": true, "utm_content": true,
}

// SearchHasSiteFilter — заданы ли в поисковой ссылке сужающие фильтры маркетплейса
// (категория, бренд, цена и пр.), помимо самого текста запроса. Эвристика: любой
// query-параметр вне searchNonFilterParams ИЛИ категорийный путь Я.Маркета. Нужно,
// чтобы не советовать «добавь фильтры», когда они уже есть. Ошибаемся в безопасную
// сторону: незнакомый трекинг-параметр → решим, что фильтр есть, и просто не
// покажем совет (лучше не надоесть, чем надоесть зря).
func SearchHasSiteFilter(rawURL string) bool {
	u, err := url.Parse(rawURL)
	if err != nil {
		return false
	}
	p := strings.ToLower(u.Path)
	if strings.Contains(p, "catalog--") || strings.Contains(p, "category--") {
		return true // категорийная страница Я.Маркета
	}
	// RawQuery парсим вручную (по «&»): url.Query() отбрасывает пары с «;» —
	// а WB-фильтры бывают вида priceU=1000;5000 / f5023=a;b;c.
	for _, pair := range strings.Split(u.RawQuery, "&") {
		key := pair
		if i := strings.IndexByte(pair, '='); i >= 0 {
			key = pair[:i]
		}
		if key == "" {
			continue
		}
		if !searchNonFilterParams[strings.ToLower(key)] {
			return true
		}
	}
	return false
}

// AppendTextFilter дописывает клиентский текст-фильтр tb_q к ссылке. RawQuery
// дополняем напрямую (не через url.Query/Encode), чтобы не потерять фильтры WB
// вида f5023=a;b;c — стандартный парсер отбрасывает пары с «;».
func AppendTextFilter(rawURL, text string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return rawURL
	}
	pair := tbTextFilterParam + "=" + url.QueryEscape(text)
	if u.RawQuery == "" {
		u.RawQuery = pair
	} else {
		u.RawQuery += "&" + pair
	}
	return u.String()
}

// ParsePrice — цена из пользовательского ввода («59 990», «59990,50», «100 ₽»).
func ParsePrice(s string) (float64, error) {
	s = strings.TrimSpace(strings.ReplaceAll(s, ",", "."))
	s = strings.ReplaceAll(s, " ", "")
	s = strings.TrimSuffix(s, "₽")
	s = strings.TrimSuffix(s, "руб")
	v, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	if err != nil || v <= 0 {
		return 0, errors.New("invalid price")
	}
	return v, nil
}

// ParsePct — процент скидки 1–99 («20», «20%»).
func ParsePct(s string) (int16, error) {
	s = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(s), "%"))
	v, err := strconv.Atoi(s)
	if err != nil || v < 1 || v > 99 {
		return 0, errors.New("invalid pct")
	}
	return int16(v), nil
}
