package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"html/template"
	"strconv"
	"strings"
	"sync"
	"time"

	"gitlab.com/KosovAndrey/tryberrybot/internal/domain"
	"gitlab.com/KosovAndrey/tryberrybot/internal/repository/postgres"
)

// templateFuncs — форматтеры для SSR-шаблона.
var templateFuncs = template.FuncMap{
	"rub": rubFmt, // 1234.5 → "1 235 ₽"
}

// rubFmt — цена без копеек, разряды разделены узким неразрывным пробелом.
func rubFmt(v float64) string {
	if v <= 0 {
		return "—"
	}
	s := strconv.FormatFloat(v, 'f', 0, 64)
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	var b strings.Builder
	for i, r := range s {
		if i > 0 && (len(s)-i)%3 == 0 {
			b.WriteString(" ") // narrow no-break space
		}
		b.WriteRune(r)
	}
	res := b.String() + " ₽"
	if neg {
		res = "-" + res
	}
	return res
}

// ── серия для графика ────────────────────────────────────────────────────────

// seriesResp — JSON-формат серии (встраивается в SSR и отдаётся /api/price-history).
// Points: [tsMillisUTC, price]. Время в ms (влезает в float64 < 2^53), цену рисуем
// ступенькой; последняя точка — синтетическая на now, чтобы текущий сегмент
// дотягивался до «сейчас».
type seriesResp struct {
	Range   string       `json:"range"`
	From    int64        `json:"from"`
	To      int64        `json:"to"`
	Current float64      `json:"current"`
	Min     float64      `json:"min"`
	Max     float64      `json:"max"`
	Points  [][2]float64 `json:"points"`
}

func buildSeries(points []postgres.PricePoint, current float64, now time.Time, rng string, from time.Time) seriesResp {
	resp := seriesResp{
		Range:   rng,
		From:    from.UnixMilli(),
		To:      now.UnixMilli(),
		Current: current,
		Points:  make([][2]float64, 0, len(points)+1),
	}
	min, max := 0.0, 0.0
	for i, p := range points {
		resp.Points = append(resp.Points, [2]float64{float64(p.RecordedAt.UnixMilli()), p.Price})
		if i == 0 || p.Price < min {
			min = p.Price
		}
		if i == 0 || p.Price > max {
			max = p.Price
		}
	}
	// Дотягиваем последний сегмент до now (цена держится до следующей смены).
	if n := len(points); n > 0 {
		last := points[n-1]
		if last.RecordedAt.Before(now) {
			resp.Points = append(resp.Points, [2]float64{float64(now.UnixMilli()), last.Price})
		}
	}
	resp.Min, resp.Max = min, max
	return resp
}

// ── диапазоны ────────────────────────────────────────────────────────────────

func normalizeRange(s string) string {
	switch s {
	case "30d", "90d", "365d", "all":
		return s
	default:
		return defaultRange
	}
}

// rangeStart — левая граница окна. "all" уходит к моменту начала наблюдения
// (created_at товара) — раньше истории всё равно нет.
func rangeStart(rng string, createdAt, now time.Time) time.Time {
	switch rng {
	case "30d":
		return now.AddDate(0, 0, -30)
	case "365d":
		return now.AddDate(0, 0, -365)
	case "all":
		if !createdAt.IsZero() {
			return createdAt.Add(-time.Hour)
		}
		return now.AddDate(-5, 0, 0)
	default: // 90d
		return now.AddDate(0, 0, -90)
	}
}

// ── данные страницы (SSR) ────────────────────────────────────────────────────

type pageData struct {
	PublicID         string
	Name             string
	MarketplaceLabel string
	Marketplace      string
	ProductURL       string
	ImageURL         string
	Canonical        string
	Title            string
	Description      string

	Current  float64
	HasPrice bool
	InStock  bool

	// Вердикт честной цены — тезис страницы (domain.AssessHonestPrice).
	VerdictClass string // good | typical | high | none
	VerdictTitle string
	VerdictNote  string

	HasData  bool
	Min30    float64
	Median30 float64
	Min90    float64
	MinAll   float64

	// Опорные линии для графика (рисует chart.js): минимум за всё время и
	// «обычная» цена = медиана 30д. 0 → линию не рисуем.
	RefMin   float64
	RefUsual float64

	SinceISO   string
	UpdatedISO string

	// JSON-LD
	LDJSON template.JS

	SeriesJSON   template.JS
	AssetVersion string // ?v= к css/js (сброс кэша при деплое)
}

// verdictPresentation переводит вердикт честной цены в подачу на странице
// (класс для семантического цвета + заголовок-тезис + пояснение). Тексты — на
// стороне пользователя: что это значит для решения «брать сейчас или нет».
func verdictPresentation(hp domain.HonestPrice) (class, title, note string) {
	switch hp.Verdict {
	case domain.VerdictLowestEver:
		return "good", "Сейчас выгодно", "Дешевле не было за всё время наблюдения."
	case domain.VerdictLowest90:
		return "good", "Хорошая цена", "Минимум за последние 90 дней."
	case domain.VerdictLowest30:
		return "good", "Хорошая цена", "Минимум за последние 30 дней."
	case domain.VerdictTypical:
		return "typical", "Обычная цена", "Столько этот товар стоит как правило — не переплата, но и не скидка."
	case domain.VerdictAboveTypical:
		return "high", "Дороже обычного", fmt.Sprintf("Сейчас выше обычной цены (%s) — «скидка» завышена.", rubFmt(hp.Median30))
	default:
		return "none", "Собираем историю", "Пока мало данных, чтобы судить о цене. Загляните позже."
	}
}

func (h *WebHandlers) buildPageData(
	p *domain.Product, inStock bool, current float64, stats domain.PriceStats,
	canonical string, now time.Time, seriesJSON template.JS,
) pageData {
	label := marketplaceLabels[p.Marketplace]
	if label == "" {
		label = p.Marketplace
	}

	title := p.Name + " — история цены"
	if label != "" {
		title += " на " + label
	}
	desc := "Динамика цены «" + p.Name + "»"
	if label != "" {
		desc += " на " + label
	}
	desc += ": график, минимум и обычная цена по наблюдениям TryBerry."

	low := stats.MinAll
	high := current
	if stats.Median30 > high {
		high = stats.Median30
	}
	if high <= 0 {
		high = current
	}

	ld := map[string]any{
		"@context": "https://schema.org/",
		"@type":    "Product",
		"name":     p.Name,
		"url":      canonical,
	}
	if p.ImageURL != "" {
		ld["image"] = p.ImageURL
	}
	if current > 0 {
		avail := "https://schema.org/InStock"
		if !inStock {
			avail = "https://schema.org/OutOfStock"
		}
		ld["offers"] = map[string]any{
			"@type":         "AggregateOffer",
			"priceCurrency": "RUB",
			"lowPrice":      low,
			"highPrice":     high,
			"offerCount":    1,
			"availability":  avail,
		}
	}
	ldJSON, _ := json.Marshal(ld)

	vClass, vTitle, vNote := verdictPresentation(domain.AssessHonestPrice(current, stats, now))

	pd := pageData{
		PublicID:         p.PublicID,
		Name:             p.Name,
		MarketplaceLabel: label,
		Marketplace:      p.Marketplace,
		ProductURL:       p.URL,
		ImageURL:         p.ImageURL,
		Canonical:        canonical,
		Title:            title,
		Description:      desc,
		Current:          current,
		HasPrice:         current > 0,
		InStock:          inStock,
		VerdictClass:     vClass,
		VerdictTitle:     vTitle,
		VerdictNote:      vNote,
		HasData:          stats.HasData,
		Min30:            stats.Min30,
		Median30:         stats.Median30,
		Min90:            stats.Min90,
		MinAll:           stats.MinAll,
		RefMin:           stats.MinAll,
		RefUsual:         stats.Median30,
		UpdatedISO:       p.UpdatedAt.UTC().Format(time.RFC3339),
		LDJSON:           template.JS(ldJSON),
		SeriesJSON:       seriesJSON,
		AssetVersion:     h.assetVersion,
	}
	if !stats.Since.IsZero() {
		pd.SinceISO = stats.Since.UTC().Format("2006-01-02")
	}
	return pd
}

func (h *WebHandlers) render(pd pageData) ([]byte, error) {
	var buf bytes.Buffer
	if err := h.tmpl.ExecuteTemplate(&buf, "product.html", pd); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ── slug / xml ───────────────────────────────────────────────────────────────

var translitMap = map[rune]string{
	'а': "a", 'б': "b", 'в': "v", 'г': "g", 'д': "d", 'е': "e", 'ё': "e",
	'ж': "zh", 'з': "z", 'и': "i", 'й': "y", 'к': "k", 'л': "l", 'м': "m",
	'н': "n", 'о': "o", 'п': "p", 'р': "r", 'с': "s", 'т': "t", 'у': "u",
	'ф': "f", 'х': "h", 'ц': "ts", 'ч': "ch", 'ш': "sh", 'щ': "sch",
	'ъ': "", 'ы': "y", 'ь': "", 'э': "e", 'ю': "yu", 'я': "ya",
}

// slugify — декоративный SEO-слаг из имени: транслит кириллицы + kebab из [a-z0-9].
// Резолв страницы всё равно по public_id, поэтому слаг не обязан быть уникальным
// и может «протухнуть» при смене имени — это нормально (canonical поправит).
func slugify(name string) string {
	var b strings.Builder
	prevDash := false
	for _, r := range strings.ToLower(name) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			prevDash = false
		case translitMap[r] != "":
			b.WriteString(translitMap[r])
			prevDash = false
		default:
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
		if b.Len() >= 60 {
			break
		}
	}
	return strings.Trim(b.String(), "-")
}

func xmlEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&quot;", "'", "&apos;")
	return r.Replace(s)
}

// ── in-proc TTL-кэш ──────────────────────────────────────────────────────────

// ttlCache — крошечный кэш на map+RWMutex. Снимает нагрузку с БД при всплесках и
// краулерах. Значения иммутабельны (готовые []byte), TTL общий. Объём мал
// (сотни товаров × несколько диапазонов), поэтому без вытеснения — лениво чистим
// просрочку при чтении.
type ttlCache struct {
	mu    sync.RWMutex
	pages map[string]cacheEntry // ключ: public_id → готовый HTML
	serie map[string]cacheEntry // ключ: public_id|range → JSON серии
	smap  cacheEntry            // sitemap.xml
}

type cacheEntry struct {
	body []byte
	exp  time.Time
}

func newTTLCache() *ttlCache {
	return &ttlCache{
		pages: make(map[string]cacheEntry),
		serie: make(map[string]cacheEntry),
	}
}

func (c *ttlCache) get(m map[string]cacheEntry, key string) ([]byte, bool) {
	c.mu.RLock()
	e, ok := m[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(e.exp) {
		return nil, false
	}
	return e.body, true
}

func (c *ttlCache) set(m map[string]cacheEntry, key string, body []byte, ttl time.Duration) {
	c.mu.Lock()
	m[key] = cacheEntry{body: body, exp: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *ttlCache) getPage(id string) ([]byte, bool)      { return c.get(c.pages, id) }
func (c *ttlCache) setPage(id string, body []byte)        { c.set(c.pages, id, body, webCacheTTL) }
func (c *ttlCache) getSeries(id, r string) ([]byte, bool) { return c.get(c.serie, id+"|"+r) }
func (c *ttlCache) setSeries(id, r string, body []byte)   { c.set(c.serie, id+"|"+r, body, webCacheTTL) }

func (c *ttlCache) getSitemap() ([]byte, bool) {
	c.mu.RLock()
	e := c.smap
	c.mu.RUnlock()
	if e.body == nil || time.Now().After(e.exp) {
		return nil, false
	}
	return e.body, true
}

func (c *ttlCache) setSitemap(body []byte) {
	c.mu.Lock()
	c.smap = cacheEntry{body: body, exp: time.Now().Add(time.Hour)}
	c.mu.Unlock()
}
