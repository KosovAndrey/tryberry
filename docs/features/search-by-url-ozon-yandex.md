# Поиск по ссылке-выдаче для Ozon + Яндекс.Маркет

Статус: в работе (ветка `feat/search-by-url-ozon-yandex`).

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
- `YandexMarketSearchScraper` (internal/scraper/yandex_market_search.go):
  встраивает карточный `YandexMarketScraper` (тот же tls-client + RU-прокси).
  `MatchesSearch`/`NormalizeSearchURL` — готовы и покрыты тестами (text + hid,
  карточки отсекаются). `ScrapeSearch` — **FIRST-PASS**: тянет HTML, отсекает
  SmartCaptcha, вытаскивает цены из сниппетов стейта
  (`"price":{"value":...,"currency":"RUR"}`) + диагностический WARN
  (`price_hits`/`price_ctx`/`cur_ctx`) для доводки по прод-логам.
- `OzonSearchScraper` (internal/scraper/ozon_search.go): `MatchesSearch`/
  `NormalizeSearchURL` готовы; `ScrapeSearch` → `ErrMarketplaceBlocked`.
- Регистрация: bot-worker и search-worker (оба МП). Бот при Ozon-search-ссылке
  показывает «Ozon скоро будет» (не заводит мёртвую подписку).
- Тесты: разбор/нормализация URL обоих МП, извлечение цен (first-pass).

## Заблокировано
- **Ozon-поиск**: (1) прямой composer закрыт FAB; (2) сайдкар ozon-miner умеет
  только `/scrape?id=<товар>` — нет search-маршрута. Нужно: search-endpoint в
  сайдкаре (composer `searchResultsV2`) + рабочий FAB. До этого ScrapeSearch
  возвращает blocked.

## TODO (доводка по прод-логам)
- **Яндекс `parseSearch`**: сейчас достаёт только цены (Position+PriceKopecks).
  Нужны Name/URL/ArticleID/ImageURL из стейта marketfront — структуру снять с
  реального payload через прод-лог `yandex search: no items parsed` (поля
  price_ctx/cur_ctx) или сохранённый HTML страницы. Без Name/URL уведомления
  бесполезны, поэтому до доводки парсера фичу НЕ включать пользователям.
- UX-строки `sendSearchMenu`/часть сообщений всё ещё «Wildberries»-центричны —
  обновить на мультимаркет, когда Яндекс-парсер дойдёт до рабочего состояния.
- Возможен переход с HTML-стейта на внутренний search-API Яндекса (надёжнее
  регэкспа), если найдём стабильные параметры запроса.
