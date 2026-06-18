# Яндекс.Маркет — статус интеграции (журнал + точка возобновления)

Ветка: **`feat/yandex-market-scraper`**.
Дата последней работы: **2026-06-17**.

## Цель
Получать цену/название/картинку товара Я.Маркета по URL карточки (как WB/Ozon),
для трекинга в боте. Ограничение Андрея: **постараться без аккаунта** (в отличие
от Ozon, где пришлось ходить под залогиненной сессией).

## ИТОГ ОДНОЙ СТРОКОЙ
**РАБОТАЕТ в проде (2026-06-17).** Боевой `/track` по `market.yandex.ru/card/...`
отдаёт цену+имя+фото. Антибот (SmartCaptcha) проходим **анонимно, без аккаунта**:
`tls-client` (Chrome-профиль) + RU-мобильный прокси (делит `OZON_PROXY_URL`) + парс
встроенного JSON-LD карточки. Цена публичная — аккаунт не нужен. Дальше — наблюдение
за устойчивостью на общем IP (см. «НАБЛЮДЕНИЕ»).

## НАБЛЮДЕНИЕ (вотч 17–19.06)
Скорость намеренно низкая: scheduler троттлит Я.Маркет как Ozon (≥`OZON_MIN_INTERVAL_MINUTES`,
reseller исключён) — бережём общий мобильный IP (его делят ozon-miner + WB-token-miner).
Наблюдаемость **уже generic by marketplace**, `yandex_market` появляется сам:
- дашборд `scraping.json` (success rate / latency p50-p99 / requests by status / price drops);
- алерты `MarketplaceBlocked` (status=blocked = **SmartCaptcha**), `HighScrapeErrorRate`
  (ratio, ловит тихий тотальный провал), `ScrapeProxyFailing`, `HighScrapeLatency`;
- **новый статус `parse_error`** (антибот пройден, но цены нет → дрейф вёрстки/нет офферов) —
  отличаем от `blocked` и от 404 на дашборде.
Что смотрим: растёт ли `blocked` (SmartCaptcha начал челленджить общий IP) или
`parse_error` (вёрстка `/card/` поехала). Если да → отдельный residential-прокси и/или
мобильное API (план Б).

## Почему без аккаунта реально (в отличие от Ozon)
- Ozon прячет цену в storefront-API за FAB И требует **залогиненную сессию** —
  аноним не проходит вообще.
- Я.Маркет отдаёт цену **публично** во встроенном `<script type="application/ld+json">`
  (`@type:Product` → `offers.price`) прямо в HTML карточки. Аккаунт не нужен —
  нужен только проход **SmartCaptcha**.
- Старая реализация (коммит `cfd4d04`) брала именно этот JSON-LD простым
  `net/http`-GET и **работала**, пока не словила SmartCaptcha → её выключили
  (`86b1712`). Капчу словили из-за **датацентрового IP + «голого» Go-TLS**, а НЕ
  из-за отсутствия аккаунта. Лечится тем же, что у Ozon: хороший TLS + RU-прокси.

## Что сделано (этот заход)
- `internal/scraper/yandex_market.go` — `YandexMarketScraper` переписан со
  заглушки на рабочий скрейпер:
  - транспорт `bogdanfinn/tls-client`, профиль `Chrome_146` + браузерные
    заголовки/порядок (как ozon web-ветка);
  - GET карточки → парс JSON-LD (переиспользован проверенный `parseYandexMarketHTML`);
    `image` принимает и строку, и массив (`ymImage.UnmarshalJSON`);
  - детект SmartCaptcha (`isYandexCaptcha`) → `ErrMarketplaceBlocked` (отдельный
    статус для алерта, отличается от «товар не найден»);
  - 404 → `ErrProductNotFound`; без tls-client → `ErrNotImplemented` (только Matches).
  - `YandexMarketOptions{ProxyURL, RPS, Logger}` — без аккаунт-полей.
- Обвязка: `cmd/scraper` и `cmd/bot-worker` передают `YANDEX_PROXY_URL` с
  **фолбэком на `OZON_PROXY_URL`** (по умолчанию тот же мобильный IP).
- `cmd/scheduler` — Я.Маркет троттлится **как Ozon** (общий IP): `isAntibot`
  покрывает `ozon.ru` и `market.yandex.ru` → `OZON_MIN_INTERVAL_MINUTES` /
  `ozonMult` / исключение reseller-планов.
- ENV: `YANDEX_PROXY_URL` (опц.), `SCRAPER_RATE_LIMIT_RPS_YANDEX` (дефолт 2) —
  в `docker-compose.yml`, `.env.example`.
- Тесты: `internal/scraper/yandex_market_test.go` (парсер JSON-LD: массив/строка
  image, no-product, детект капчи, Matches). Зелёные.

## Метрики/статусы (уже работают через registry)
- `success` / `blocked` (SmartCaptcha) / `not_found` / `proxy` (407/502) / `error`.
- Алерты `ScrapeProxyFailing` / `MarketplaceBlocked` срабатывают по тем же
  правилам, что у Ozon (label `marketplace=yandex_market`).

## ОСТАЛОСЬ / проверить на проде
1. 🔴 **Боевая проверка через RU-прокси**: задеплоить ветку, `/track` по реальной
   ссылке `market.yandex.ru/...`, убедиться что НЕ `blocked` (SmartCaptcha пройден).
   Если капча всё же бьёт даже с хорошим TLS+IP → фаза 2 (браузер-как-транспорт,
   как у Ozon, см. ниже).
2. 🟡 **Понаблюдать выгорание общего IP**: Я.Маркет делит мобильный IP с Ozon-скрейпом
   и WB-майнерами. Если растёт `blocked`/`proxy` — завести отдельный `YANDEX_PROXY_URL`.
3. 🟢 Подстроить маркеры SmartCaptcha (`isYandexCaptcha`) по реальному ответу.
4. 🟢 Проверить устойчивость матчера URL Я.Маркета (`/product--slug/<id>`,
   `?sku=`, региональные редиректы) на реальных ссылках.

## Фаза 2 (если аноним всё же режется капчей)
Тот же план, что у Ozon: браузер-как-транспорт (Patchright+Xvfb, пул дорожек),
in-page fetch карточки из прошедшего SmartCaptcha контекста. Но сначала — спайк:
простой tls-client+RU-прокси (текущий код) может оказаться достаточным, т.к.
аккаунт не нужен и капча у Я.Маркета мягче ozon-FAB.

## Открытые вопросы / риски
- **Стоимость**: прокси переиспользуем (0 доп. при дележе IP) — но дележ жжёт IP
  быстрее. Отдельная подписка ~1790₽/мес при росте нагрузки.
- **SmartCaptcha может зарежить** даже хороший TLS+IP при высокой частоте → пол
  интервала (как Ozon) уже стоит; держать RPS низким.
- **JSON-LD может пропасть** (A/B вёрстки Я.Маркета) → парсер вернёт `not_found`;
  фолбэк (мобильный BFF-API) — на будущее.

## Безопасность
- Аккаунт не нужен → нет риска бана/засветки сессии (плюс к Ozon).
- `YANDEX_PROXY_URL` — секрет в `.env` (в `.gitignore`), в логи пишем только
  `proxy=true/false`.
