# Короткие ссылки из мобильных приложений (url shortener)

**Ветка:** `feat/short-links` · **Дата:** 2026-06-23

## Проблема

Кнопка «Поделиться» в приложениях Ozon / AliExpress отдаёт не товарный URL, а
короткую ссылку-редиректор:

- Ozon: `https://ozon.ru/t/XXXXXXX`
- AliExpress: `https://a.aliexpress.com/_XXXXXX` (мобильный шэр),
  `https://s.click.aliexpress.com/…` (партнёрский), `https://aliexpress.ru/e/_X`

Ни один скрейпер их не матчит (`OzonScraper.Matches` ждёт `ozon.ru/product/`,
`AliexpressScraper.Matches` — `aliexpress.ru/item/`). В `handleMessage` такая
ссылка проходит мимо всех гейтов (bulk / поиск / товар / продавец) и **падает в
главное меню** — пользователь теряется на первом же контакте.

Wildberries и Я.Маркет в приложениях отдают уже полный товарный URL
(`wildberries.ru/catalog/…/detail.aspx`, `market.yandex.ru/…`), он матчится
напрямую — шортнера у них в обиходе нет.

## Решение

`internal/scraper/resolver.go` — `LinkResolver`. Перед гейтами распознавания
разворачиваем короткую ссылку по 3xx-редиректам до канонического товарного URL,
который дальше штатно подхватывает `Registry.FindByURL`.

Ключевые решения:

1. **Только allowlist хостов** (`shortLinkPatterns`), а не любой http-URL из
   текста. Иначе бот слал бы исходящий запрос на произвольный хост, вставленный
   пользователем (SSRF / шум). Для обычных ссылок и текста без шортнеров —
   **нулевая сеть**.
2. **Direct egress (`Proxy: nil`).** Клиент резолвера НЕ наследует `HTTPS_PROXY`
   из окружения. В `bot-worker` он указывает на `xray` (немецкий VLESS-exit для
   egress в Telegram) — гонять через него RU-редиректоры неверно по гео. Сам
   редирект отдаётся как 3xx+`Location` без антибота, IP-репутация для него не
   нужна; тяжёлую товарную страницу резолвер не тянет — её скрейпит уже нужный
   скрейпер со своим egress (Ozon → `OZON_PROXY_URL`, Ali → direct/proxy-jar).
3. **Fail-open:** при любой ошибке (таймаут, сеть, битый редирект) возвращаем
   исходный URL — пусть его честно отвергнет `FindByURL`, а не молча проглотит.
4. **Кап редиректов** = 8 (`CheckRedirect` → `ErrUseLastResponse`), таймаут 8с.

### Точки интеграции (`internal/telegram/bot.go`)

- `handleMessage`: `text = b.resolver.ExpandInText(ctx, text)` — после FSM-гейтов
  (email/порог/код привязки НЕ трогаем) и **до** гейтов bulk/поиск/товар. Один
  вызов покрывает все текстовые флоу. Развёрнутый URL уходит и в `doTrack`
  (одиночный), и в `trackOne` (bulk) → `Upsert(rawURL)` сохраняет канонический
  URL, фоновый скрейпер не спотыкается на короткой ссылке каждый цикл.
- `handleCommand` `case "track"`: `args = b.resolver.ExpandInText(ctx, args)`
  перед `doTrack` — команда `/track <короткая ссылка>` минует гейты handleMessage.

Резолвер собирается в `NewBot` без новых параметров (`scraper.NewLinkResolver(0)`).

## Тесты

`internal/scraper/resolver_test.go` — герметичные, через инъекцию fake
`RoundTripper` в `r.client.Transport` (loopback-сокеты в песочнице/CI недоступны).
Покрыто: allowlist-гейтинг (`isShortLink`), многохоповая цепочка редиректов,
fail-open на сетевой ошибке, отсутствие сети для не-шортнеров и текста без ссылок,
сохранение текста вокруг ссылки в `ExpandInText`.

## Открытый вопрос — egress для Ozon `/t/`

Резолв идёт **напрямую с IP сервера**. Для AliExpress (`a.aliexpress.com`) это
почти наверняка ок — редиректор отдаёт 302 без проверки. Для Ozon `ozon.ru/t/`
есть риск: Ozon агрессивен к датацентр-IP и может вернуть не 302, а
challenge-страницу (200) — тогда `resp.Request.URL` останется на короткой ссылке,
`FindByURL` не распознает, пользователь получит «не распознал».

**Проверка на проде после деплоя** — реальной share-ссылкой из каждого приложения:
`/track https://ozon.ru/t/<…>` и `/track https://a.aliexpress.com/_<…>`. Если
Ozon не разворачивается — следующий шаг: для `ozon.ru/t/` резолвить через
`OZON_PROXY_URL` (RU-мобильный), как это делает сам Ozon-скрейпер. Структура
резолвера это допускает (отдельный клиент под Ozon-паттерны).
