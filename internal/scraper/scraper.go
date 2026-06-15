package scraper

import (
	"context"
	"errors"
)

// Marketplace — идентификатор маркетплейса (используется в БД и логах)
type Marketplace string

const (
	MarketplaceWildberries  Marketplace = "wildberries"
	MarketplaceOzon         Marketplace = "ozon"
	MarketplaceYandexMarket Marketplace = "yandex_market"
)

// Result — единый формат данных о товаре, независимый от маркетплейса
type Result struct {
	Name     string
	Price    float64
	ImageURL string
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
)
