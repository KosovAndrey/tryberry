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

// Label — как площадка называется в текстах для пользователя. Раньше названия
// были рассыпаны по каналам строками, и после подключения Ozon/YM/Ali часть из
// них осталась врать про «только Wildberries».
func (m Marketplace) Label() string {
	switch m {
	case MarketplaceWildberries:
		return "Wildberries"
	case MarketplaceOzon:
		return "Ozon"
	case MarketplaceYandexMarket:
		return "Яндекс.Маркет"
	case MarketplaceAliexpress:
		return "AliExpress"
	default:
		return string(m)
	}
}

// Result — единый формат данных о товаре, независимый от маркетплейса
type Result struct {
	Name     string
	Price    float64
	ImageURL string
	// InStock — есть ли активный оффер/цена. false + Price==0 означает «карточка
	// товара валидна, но сейчас не продаётся» (нет buy-box). Скрейперы с ценой
	// всегда ставят true; ситуацию «нет оффера» отдают Я.Маркет и WB.
	InStock bool

	// StockUnknown — источник цены НЕ ЗНАЕТ про наличие, и InStock у него выдуман.
	// Так устроен архив WB (basket-CDN): он хранит последнюю известную цену, но про
	// запас не знает ничего. Раньше этот путь безусловно объявлял «в наличии», и
	// пропавший товар навсегда застревал с распродажной ценой — она по построению
	// становилась минимумом истории, а дайджест советовал купить то, чего нет
	// (инцидент 01-09-2026, ~25% скрейпов WB шли через архив). Флаг велит НЕ
	// трогать сохранённое наличие: пусть решает источник, который его действительно
	// видит. Ноль значения безопасен — обычные скрейперы наличие знают.
	StockUnknown bool

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

// DeadURLFormMsg — что сказать пользователю, приславшему ссылку в форме, которую
// площадка больше не открывает. Текст общий для всех трёх ботов (TG/VK/MAX): у нас
// паритет каналов, и разъехавшиеся формулировки на одном и том же отказе — баг.
const DeadURLFormMsg = "🔗 Эта ссылка Я.Маркета в старом формате — Маркет её больше не открывает.\n\n" +
	"Открой товар на market.yandex.ru и пришли адрес из адресной строки. " +
	"Рабочая ссылка выглядит так: market.yandex.ru/card/…"

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
	// ErrDeadURLForm — ссылка в форме, которую площадка больше не обслуживает.
	// Не блокировка и не «нет товара»: сама карточка жива, но по ЭТОМУ адресу её
	// не отдают. Возвращается ДО похода в сеть, поэтому такие ссылки не греют
	// брейкер (иначе один мёртвый URL раз за разом капчился бы и глушил площадку
	// целиком — ровно это случилось с Я.Маркетом 01-09-2026).
	//
	// Пока единственный случай: формы /product/<modelId> и /product--<slug>/<modelId>
	// Я.Маркета, закрытые SmartCaptcha. Живая — /card/<slug>/<oskuId>.
	// docs/YANDEX-CARD-MIGRATION.md.
	ErrDeadURLForm = errors.New("marketplace no longer serves this URL form")
)
