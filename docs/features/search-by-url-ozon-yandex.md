# Поиск по ссылке-выдаче для Ozon + Яндекс.Маркет

Статус: Я.Маркет и Ozon готовы и включены пользователям
(ветка `feat/search-by-url-ozon-yandex`).

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

### Ozon — ГОТОВ, включён пользователям
- Сайдкар `ozon-miner` (server.py): in-page fetch обобщён на произвольный
  entrypoint-path (`_FETCH_JS`), добавлен маршрут `GET /search?text=...` —
  дёргает `/search/?text=...` из той же прогретой дорожки (новый прогрев не
  нужен: fetch same-origin, куки доменные), отдаёт сырой widgetStates.
  `Pool.pick_any` — любая живая дорожка.
- `OzonSearchScraper.ScrapeSearch`: ходит в `{browserURL}/search?text=...`
  (зеркало `fetchViaBrowser`), парсит тайлы виджета `tileGridDesktop`
  (`parseSearch`/`buildOzonSearchItem`). Структура тайла **подтверждена
  прод-дампом** (text=iphone, 8/8 позиций): `id`/`sku` → SKU, `action.link` → URL,
  `mainState` priceV2 (PRICE/ORIGINAL_PRICE) → цена, textDS `id=="name"` →
  название, `tileImage` → картинка (reuse `findPriceTexts`/`findOzonImageURL`).
  Работает только в browser-режиме (mode=browser + OZON_BROWSER_URL), иначе →
  blocked. search-worker поднимает Ozon-базу в browser-режиме.
- Гейт «Ozon скоро будет» в боте снят — поиск-подписки Ozon заводятся как WB/Я.М.
- Тесты: разбор/нормализация URL обоих МП, парсеры Я.Маркета и Ozon
  (на реальной структуре тайла: SKU/цена/old/name/image/дедуп).

### Доводка по дампу (как делали оба МП)
Если разметка дрейфанёт (0 тайлов / нет цены) — снять свежий дамп на проде:
`docker exec pt_ozon_miner python3 -c "..."` → `http://localhost:8080/search?text=...`
изнутри контейнера (портов наружу нет). Диаг-лог `ozon search: no items parsed`
(widgets+sample) подскажет, какой виджет смотреть.

## Возможные улучшения
- Переход с HTML-стейта Я.Маркета на внутренний search-API (надёжнее регэкспа),
  если найдём стабильные параметры запроса.
- Отдельный residential/мобильный прокси под Я.Маркет при росте нагрузки (сейчас
  делит IP с Ozon, лимитер 0.5 rps).
