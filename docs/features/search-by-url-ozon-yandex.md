# Поиск по ссылке-выдаче для Ozon + Яндекс.Маркет

Статус: Я.Маркет готов и включён; Ozon — каркас готов, парсер доводится по
прод-логам (ветка `feat/search-by-url-ozon-yandex`).

Цель: как у WB — подписка не на карточку, а на поисковую выдачу по ссылке
(`?text=...`), с уведомлением о подешевевших товарах.

## Архитектура (уже была)
Весь пайплайн поиск-подписок (search-worker, search_subscriptions, дедуп по
normalized_url, Kafka search-events, Telegram UI, движок Decide) —
маркетплейс-агностичен. Добавление МП = реализация интерфейса
`scraper.SearchScraper` (`MatchesSearch` / `NormalizeSearchURL` / `ScrapeSearch`)
+ регистрация в реестрах **bot-worker** (только разбор URL) и **search-worker**
(реальный скрейп). Downstream не трогается.

## Сделано
### Яндекс.Маркет — ГОТОВ, включён пользователям
- `YandexMarketSearchScraper` (internal/scraper/yandex_market_search.go):
  встраивает карточный `YandexMarketScraper` (тот же tls-client + RU-прокси).
  `MatchesSearch`/`NormalizeSearchURL` (text + hid, карточки отсекаются).
  `parseSearch` достаёт полноценные позиции из инлайн-стейта marketfront —
  ArticleID, Name (`titles.raw`), URL (`/product--<slug>/<id>`),
  PriceKopecks+OldPrice, ImageURL (резолв хэшей картинок). Метод: якорь
  `{"id":N,"entity":"product"`, вычитка сбалансированного JSON-объекта
  (`ymBalancedObject`) + `json.Unmarshal` (устойчиво к дрейфу порядка полей).
- UX: меню «Поиск по ссылке» и пустой список упоминают Я.Маркет; иконка МП в
  списке поиск-подписок.

### Ozon — КАРКАС готов, парсер best-effort (доводка по логам)
- Сайдкар `ozon-miner` (server.py): in-page fetch обобщён на произвольный
  entrypoint-path (`_FETCH_JS`), добавлен маршрут `GET /search?text=...` —
  дёргает `/search/?text=...` из той же прогретой дорожки (новый прогрев не
  нужен: fetch same-origin, куки доменные), отдаёт сырой widgetStates с виджетом
  `searchResultsV2`. `Pool.pick_any` — любая живая дорожка.
- `OzonSearchScraper.ScrapeSearch`: ходит в `{browserURL}/search?text=...`
  (зеркало `fetchViaBrowser`), парсит тайлы выдачи (`parseSearch`): link→SKU,
  цена по textStyle (reuse `findPriceTexts`/`parseRubles`), URL/Name/Image
  best-effort. Работает только в browser-режиме (mode=browser + OZON_BROWSER_URL),
  иначе → blocked. search-worker поднимает Ozon-базу в browser-режиме.
- Тесты: разбор/нормализация URL обоих МП, парсер Я.Маркета, плумбинг парсера
  Ozon (items→link→price→URL→дедуп) на синтетическом widgetStates.

## Доводка Ozon-парсера (как делали Я.Маркет)
Точная структура `searchResultsV2` на живой странице не зафиксирована (локально
FAB). Дамп снять прямо на проде: `curl 'http://ozon-miner:8080/search?text=наушники'`
(из сети compose) → по реальным именам полей item'а поправить `buildOzonSearchItem`
/`findOzonTileTitle`. Диаг-лог `ozon search: no items parsed` (widgets+sample)
помогает, если 0 тайлов. **Бот пока показывает «Ozon скоро будет»** (гейт в
telegram/search.go) — снять ПОСЛЕ подтверждения парсера на проде.

## Возможные улучшения
- Переход с HTML-стейта Я.Маркета на внутренний search-API (надёжнее регэкспа),
  если найдём стабильные параметры запроса.
- Отдельный residential/мобильный прокси под Я.Маркет при росте нагрузки (сейчас
  делит IP с Ozon, лимитер 0.5 rps).
