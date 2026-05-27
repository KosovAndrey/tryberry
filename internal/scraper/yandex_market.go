package scraper

import (
	"context"
	"strings"
)

// YandexMarketScraper — временная заглушка.
//
// Реальный скрейпинг Я.Маркета невозможен простыми HTTP-запросами:
// market.yandex.ru защищён SmartCaptcha и при попытке скачать HTML карточки
// возвращает страницу капчи вместо контента (проверено — см. SCRAPER.md).
//
// Чтобы вернуть скрейпинг, нужны: headless-браузер (chromedp/playwright)
// либо прокси через резидентные IP, либо комбо первого со вторым.
// До этого скрейпер сразу возвращает ErrNotImplemented — бот покажет
// пользователю понятное сообщение "Маркетплейс пока не поддерживается".
//
// Конструктор принимает rps чтобы сигнатура совпадала с прошлой реализацией —
// меньше изменений в местах регистрации scraper'а.
type YandexMarketScraper struct{}

func NewYandexMarketScraper(_ float64) *YandexMarketScraper {
	return &YandexMarketScraper{}
}

func (s *YandexMarketScraper) Marketplace() Marketplace {
	return MarketplaceYandexMarket
}

func (s *YandexMarketScraper) Matches(url string) bool {
	return strings.Contains(url, "market.yandex.ru/")
}

func (s *YandexMarketScraper) Scrape(_ context.Context, _ string) (*Result, error) {
	return nil, ErrNotImplemented
}
