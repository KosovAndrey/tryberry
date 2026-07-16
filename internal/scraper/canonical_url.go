package scraper

import "fmt"

// CanonicalProductURL приводит товарный URL к единственной форме на товар.
//
// Зачем: products.url — UNIQUE, и это де-факто КЛЮЧ товара. Одна и та же карточка
// приходит в бот десятком написаний (со slug и без, с ?asb=/utm/, с www и без,
// из мобильного шаринга), и каждое заводило ОТДЕЛЬНЫЙ products — со своей
// историей цен, своими подписками и своим public_id. Пример 2026-07-15:
// /product/2274265393/ создал дубль 9352881 рядом с 8204070.
//
// Канон = id товара, потому что именно он идентифицирует карточку у самого
// маркетплейса. Переписывать URL безопасно ровно потому, что WB/Ozon/Ali его НЕ
// загружают: Scrape достаёт из него id (ExtractArticleID/extractOzonID/
// ExtractAliexpressID) и дальше работает по id, а сам URL живёт только как ключ
// и ссылка для показа юзеру.
//
// Я.Маркет НАМЕРЕННО не трогаем: он единственный, кто реально фетчит сохранённый
// URL, да ещё и достаёт из его пути sku (ymExtractSKU), которым потом вытаскивает
// цену из HTML (ymStatePrice). Канон для него = отдельная задача с проверкой, что
// свёрнутая форма и открывается, и парсится. Дубли YM пока остаются.
//
// URL, из которого id не достаётся, возвращается КАК ЕСТЬ: пусть лучше останется
// дубль, чем мы потеряем рабочую ссылку.
func CanonicalProductURL(m Marketplace, rawURL string) string {
	switch m {
	case MarketplaceWildberries:
		if id, err := ExtractArticleID(rawURL); err == nil {
			return fmt.Sprintf("https://www.wildberries.ru/catalog/%s/detail.aspx", id)
		}
	case MarketplaceOzon:
		if id, err := extractOzonID(rawURL); err == nil {
			return fmt.Sprintf("https://www.ozon.ru/product/%s/", id)
		}
	case MarketplaceAliexpress:
		if id, err := ExtractAliexpressID(rawURL); err == nil {
			return fmt.Sprintf("%s/item/%s.html", aliBaseURL, id)
		}
	}
	return rawURL
}
