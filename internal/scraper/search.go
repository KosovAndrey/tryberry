package scraper

import (
	"context"
	"fmt"
)

// feedbackPointKopecks — стоимость одного балла за отзыв в копейках.
//
// Гипотеза: 1 балл = 1 рубль = 100 копеек (со слов пользователя: «1 бонус =
// 1 рубль», «900 баллов»). ЕДИНИЦА ПОЛЯ feedbackPoints В ОТВЕТЕ WB НЕ
// ПОДТВЕРЖДЕНА эмпирически — на разведке поймать ненулевое значение не удалось
// (429). Подтверждается тестом: подписка на мегафон-товар с заведомо высоким
// порогом. Если эффективная цена окажется абсурдной (например, ушла далеко в
// минус) — значит feedbackPoints приходит уже в копейках, и тогда правка ровно
// в одном месте: эта константа → 1.
const feedbackPointKopecks = 100

// SearchItem — одна позиция поисковой выдачи маркетплейса.
//
// Цены — в КОПЕЙКАХ (как отдаёт WB: рубли × 100), чтобы не терять точность
// на float. Конвертация в рубли — на границе отображения.
type SearchItem struct {
	ArticleID string // nm/id товара
	Name      string
	Brand     string
	URL       string // канонический URL карточки
	ImageURL  string
	Position  int // сквозная позиция в выдаче (1-based, через все страницы)

	PriceKopecks    int64 // финальная цена (WB price.product)
	OldPriceKopecks int64 // цена до скидки (WB price.basic), 0 если нет

	// FeedbackPointsRaw — поле feedbackPoints из ответа WB «как есть».
	// Единица не подтверждена (см. feedbackPointKopecks).
	FeedbackPointsRaw int64
}

// FeedbackPointsKopecks — баллы за отзыв, приведённые к копейкам.
func (it SearchItem) FeedbackPointsKopecks() int64 {
	return it.FeedbackPointsRaw * feedbackPointKopecks
}

// EffectivePriceKopecks — цена для срабатывания триггеров: финальная цена
// минус баллы за отзыв (баллы трактуем как условный возврат рублём).
// Никогда не отрицательная.
func (it SearchItem) EffectivePriceKopecks() int64 {
	v := it.PriceKopecks - it.FeedbackPointsKopecks()
	if v < 0 {
		return 0
	}
	return v
}

// HasFeedbackPoints — есть ли у товара ненулевые баллы за отзыв.
func (it SearchItem) HasFeedbackPoints() bool {
	return it.FeedbackPointsRaw > 0
}

// SearchResultSet — результат скрейпинга одной поисковой выдачи.
type SearchResultSet struct {
	Items      []SearchItem
	PagesRead  int // сколько страниц реально прочитали
	TotalFound int // сколько товаров WB заявил всего (для логов; 0 если неизвестно)
}

// SearchScraper — опциональный интерфейс: маркетплейс умеет скрейпить не
// только карточку товара, но и поисковую выдачу по ссылке.
//
// Реализуется не всеми скрейперами; реестр находит подходящий через
// type-assertion (см. Registry.FindSearchByURL).
type SearchScraper interface {
	MarketplaceScraper

	// MatchesSearch — относится ли URL к поисковой выдаче этого маркетплейса.
	MatchesSearch(url string) bool

	// NormalizeSearchURL — канонизировать URL для дедупликации между
	// пользователями (ключ normalized_url в БД). Возвращает ErrInvalidURL,
	// если URL не является поддерживаемой поисковой ссылкой.
	NormalizeSearchURL(url string) (string, error)

	// ScrapeSearch — получить товары выдачи (пагинация, паузы, backoff,
	// ротация прокси). Частичный результат при сбое на поздних страницах
	// допустим — то, что уже прочитано, возвращается без ошибки.
	ScrapeSearch(ctx context.Context, url string) (*SearchResultSet, error)
}

// FindSearchByURL — найти search-capable скрейпер для URL поисковой выдачи.
func (r *Registry) FindSearchByURL(url string) (SearchScraper, error) {
	for _, s := range r.scrapers {
		if ss, ok := s.(SearchScraper); ok && ss.MatchesSearch(url) {
			return ss, nil
		}
	}
	return nil, fmt.Errorf("%w: no search scraper matches URL %s", ErrInvalidURL, url)
}

// vanityResolver — скрейпер умеет резолвить буквенный слаг витрины в числовой id.
type vanityResolver interface {
	ResolveVanity(ctx context.Context, slug string) (string, error)
}

// ResolveSellerVanity — резолв буквенного слага витрины (/seller/{slug}) в
// числовой supplierId через первый способный скрейпер. ErrNotImplemented, если
// резолвер не зарегистрирован/не настроен.
func (r *Registry) ResolveSellerVanity(ctx context.Context, slug string) (string, error) {
	for _, s := range r.scrapers {
		if vr, ok := s.(vanityResolver); ok {
			return vr.ResolveVanity(ctx, slug)
		}
	}
	return "", fmt.Errorf("%w: no vanity resolver", ErrNotImplemented)
}
