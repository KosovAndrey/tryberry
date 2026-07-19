package scraper

import (
	"fmt"
	"net/url"
	"strings"
)

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
// Я.Маркет — особый случай: он единственный РЕАЛЬНО фетчит сохранённый URL
// (остальные достают id и работают по id), да ещё и берёт из пути sku
// (ymExtractSKU), которым потом вытаскивает цену (ymStatePrice). Поэтому канон
// для него не выводили из общих соображений, а ПРОВЕРИЛИ живым скрейпером
// (cmd/ym-canon-probe, 2026-07-16): 4 из 4 карточек с живой ценой дали на
// /product/<id> ту же цену и то же имя, что на /product--<slug>/<id>. Слаг
// декоративный — товар резолвится по id.
//
// Оно того стоило: у YM дублей ~1057 строк (14524 на 13460 товаров, у одного
// целых 12 копий) — слаг гуляет. Для сравнения, у Ozon дубль был ОДИН.
//
// Query-хвост (?sku=/?offerid=) срезаем: в базе он есть у 4 ссылок из 14524, а
// парсер его и так не использует (sku берётся из пути).
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
	case MarketplaceYandexMarket:
		if id, err := ExtractYandexMarketID(rawURL); err == nil {
			return fmt.Sprintf("https://market.yandex.ru/product/%s", id)
		}
		// /card/<slug>/<id> — рекламная форма из выдачи (юзеры шлют ссылки с cpc=/
		// sponsored=). id из неё НЕ канонизируется в /product/<id>: проверено на
		// прод-IP 2026-07-19 через тёплый бот — и 10-, и 12-значные id из /card/
		// не резолвятся как /product/ («не удалось получить данные»). Но САМА
		// /card/-форма рабочая (имя/цена/OOS парсятся) — рушит её только query-хвост:
		// cpc/sponsored/do-waremd5/showOriginalKmEmptyOffer протухают, и повторный
		// скрейп сохранённого URL теряет цену (товар гниёт — возраст 1–30 дней против
		// 1 мин у /product/). Поэтому канон /card/ = срезать query, путь со слагом
		// оставить (по нему ymExtractSKU берёт id для цены; slug-less /card/<id> не
		// проверен). OOS-хвост showOriginalKmEmptyOffer scraper вернёт сам при
		// надобности (ymOOSFullCardURL).
		if u, err := url.Parse(rawURL); err == nil && strings.Contains(u.Path, "/card/") {
			u.RawQuery = ""
			u.Fragment = ""
			return u.String()
		}
	}
	return rawURL
}
