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
		// ЖИВАЯ форма — /card/<slug>/<oskuId>, канон = она же без слага и без
		// query. Слаг декоративен (проверено: /card/x/<oskuId> отдаёт полную
		// карточку), но живой: он меняется при переименовании товара, а url —
		// UNIQUE-ключ, так что слаг в каноне плодил бы дубли с новой историей.
		//
		// Формы /product/<modelId> и /product--<slug>/<modelId> с 01-09-2026
		// заворачиваются на SmartCaptcha и в канон больше не годятся. Прежний
		// канон (миграции 030/031) вёл именно в /product/<id> — из-за этого лёг
		// весь Я.Маркет, 14086 ссылок. Резолв modelId → oskuId делается только
		// через выдачу, здесь его нет, поэтому /product/-ссылки возвращаем как
		// есть: пусть их честно отвергнет скрейпер, а не молча подменит канон.
		// Разбор — docs/YANDEX-CARD-MIGRATION.md.
		if u, err := url.Parse(rawURL); err == nil && strings.Contains(u.Path, "/card/") {
			if id := ymExtractSKU(rawURL); id != "" {
				return ymCardURL(id)
			}
		}
	}
	return rawURL
}

// DisplayProductURL — «красивый» адрес карточки для показа пользователю, когда он
// отличается от канона. Пусто = показывать канон.
//
// Нужен из-за Я.Маркета: канон у него намеренно без слага (/card/x/<oskuId>), а
// слаг живой и меняется при переименовании товара — держать его в UNIQUE-ключе
// значит плодить дубли с новой историей. Показывать голый /card/x/ тоже плохо: по
// ссылке не видно, что за товар. Поэтому ключ и ссылка для показа разъехались.
//
// Query срезаем: cpc/sponsored/do-waremd5/showOriginalKmEmptyOffer протухают, а
// протухший хвост ломает и повторный скрейп, и ссылку.
func DisplayProductURL(m Marketplace, rawURL string) string {
	if m != MarketplaceYandexMarket {
		return ""
	}
	u, err := url.Parse(rawURL)
	if err != nil || !strings.Contains(u.Path, "/card/") {
		return ""
	}
	// Слаг-заглушка ничего не добавляет к канону — показывать нечего.
	if strings.HasPrefix(u.Path, "/card/x/") {
		return ""
	}
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}
