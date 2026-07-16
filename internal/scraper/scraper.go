package scraper

import (
	"context"
	"errors"
	"time"
)

// Marketplace — идентификатор маркетплейса (используется в БД и логах)
type Marketplace string

const (
	MarketplaceWildberries  Marketplace = "wildberries"
	MarketplaceOzon         Marketplace = "ozon"
	MarketplaceYandexMarket Marketplace = "yandex_market"
	MarketplaceAliexpress   Marketplace = "aliexpress"
)

// Result — единый формат данных о товаре, независимый от маркетплейса
type Result struct {
	Name     string
	Price    float64
	ImageURL string
	// InStock — есть ли активный оффер/цена. false + Price==0 означает «карточка
	// товара валидна, но сейчас не продаётся» (нет buy-box). Отдают Я.Маркет и WB
	// (последний — только когда цена пришла с живой карточки через браузер-сайдкар:
	// архив basket-CDN наличия не знает в принципе и всегда даёт true).
	InStock bool

	// History — НЕОБЯЗАТЕЛЬНАЯ историческая серия цен от самого маркетплейса (для
	// бэкфилла price_history на первом скрейпе товара, чтобы график/«честная цена»
	// работали сразу). Сейчас заполняет только WB (basket-CDN price-history.json).
	// Точки в прошлом, отсортированы по времени; текущую цену добавляет обычный путь.
	History []PriceHistoryPoint
}

// PriceHistoryPoint — точка исторической серии маркетплейса (см. Result.History).
type PriceHistoryPoint struct {
	At    time.Time
	Price float64
}

// MarketplaceScraper — интерфейс который реализует каждый маркетплейс.
// Добавление нового источника = новая реализация этого интерфейса
// + регистрация в Registry. Изменения в остальном коде не требуются.
type MarketplaceScraper interface {
	// Marketplace возвращает идентификатор маркетплейса
	Marketplace() Marketplace

	// Matches проверяет относится ли URL к этому маркетплейсу
	Matches(url string) bool

	// Scrape получает данные о товаре по URL
	Scrape(ctx context.Context, url string) (*Result, error)
}

// Доменные ошибки скрейпинга — общие для всех маркетплейсов
var (
	ErrInvalidURL         = errors.New("invalid product URL")
	ErrProductNotFound    = errors.New("product not found")
	ErrMarketplaceBlocked = errors.New("marketplace blocked the request")
	ErrNotImplemented     = errors.New("marketplace not implemented yet")
	// ErrAgeRestricted — товар скрыт за возрастным гейтом 18+ (Ozon: нож, алкоголь
	// и т.п.). В widgetStates нет цены. Лечится подтверждением 18+ в настройках
	// аккаунта, под которым ходит скрейпер.
	ErrAgeRestricted = errors.New("product is age-restricted (18+)")
	// ErrAuthExpired — аккаунт-сессия скрейпера протухла (Ozon: 401 или 200 со
	// страницей логина вместо карточки). Отдельный сигнал, чтобы НЕ путать со
	// «товар не найден» и громко алертить: нужен свежий cookie / авто-рефреш токена.
	ErrAuthExpired = errors.New("scraper account session expired")
	// ErrParseFailed — страницу получили (антибот пройден, HTTP 200), но цену из неё
	// не вытащили: либо вёрстка маркетплейса изменилась (парсер отстал), либо у
	// товара нет офферов/цены. Отдельный статус "parse_error" в метрике — чтобы на
	// дашборде отличать «сломался парсер / нет цены» и от блокировки, и от 404.
	ErrParseFailed = errors.New("scraped page parsed but no price found")
)
