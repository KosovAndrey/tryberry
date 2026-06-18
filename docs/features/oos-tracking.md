# Отслеживание товаров без активного оффера (out-of-stock)

Статус: в работе (ветка `feat/yandex-market-scraper`).

## Проблема
Карточки Я.Маркета без активного buy-box (`showOriginalKmEmptyOffer=1`) не
добавлялись: `Scrape` отдавал ошибку при отсутствии цены → товар не попадал в
подписки. Нужно: добавлять такие карточки и следить, предлагая на выбор два
режима — «появилось в наличии» и «цена ниже X».

Связанный парсер-фикс (отдельно): для карточек, где JSON-LD `Product` есть, но
цена лежит в стейте marketfront (`"price":{"value":...,"currency":"RUR"}`), цена
теперь подхватывается из стейта (`ymStatePrice`). OOS-флоу включается, только
когда цены нет НИГДЕ.

## Сигнал наличия
`scraper.Result.InStock bool`. **Наличие определяется наличием `offers` в
JSON-LD, а НЕ присутствием цены где-либо.** Я.Маркет:
- JSON-LD `offers.price` есть → `InStock=true`, `Price=offers.price`.
- JSON-LD `Product` есть, но `offers` нет → пустой buy-box = **НЕ в наличии**.
  `InStock=false`, а `Price` несём как «последнюю/справочную» цену из стейта
  marketfront (`"price":{"value":...}`), 0 если её тоже нет. Стейт-цена — это
  last-known, НЕ признак наличия (важная правка: раньше стейт-цена ошибочно
  трактовалась как «в наличии»).
- нет `Product` (поиск/каталог) → ErrParseFailed.

WB/Ozon всегда `InStock=true`.

**OOS-событие в Kafka:** scraper кладёт `NewPrice=0` (даже если есть last-цена) —
иначе notifier через страховку `inStock := InStock || NewPrice>0` ложно счёл бы
товар «в наличии». Last-цена живёт в подписке (baseline/first_seen), не в событии.

## Pipeline
`PriceEvent` получает два аддитивных поля: `InStock` (текущее), `WasInStock`
(предыдущее, из `products.in_stock` до апдейта). JSON-поля аддитивны — при
rolling-деплое старые сообщения дают `false`, что лишь подавляет срабатывания
(не шлёт ложные). Нотифаер дополнительно страхуется: `inStock := InStock ||
NewPrice>0`, `wasInStock := WasInStock || OldPrice>0`.

Scraper-handler:
- читает prev `in_stock` из products, пишет новое значение;
- in stock → как раньше (price_history insert, cache, drop-метрика);
- out of stock → НЕ пишем price_history/cache (не засоряем аналитику), `NewPrice=0`;
- событие несёт `InStock`/`WasInStock`.

Notifier:
- триггер `back_in_stock`: срабатывает при переходе `!wasInStock && inStock`;
- ценовые триггеры (`below_target`/`any_drop`/`discount_pct`): оцениваются ТОЛЬКО
  когда `inStock` (иначе `0 <= target` дал бы ложное срабатывание).

## Схема (migration 019)
- `products.in_stock BOOLEAN NOT NULL DEFAULT TRUE`;
- `back_in_stock` добавлен в `chk_sub_trigger_type` (аддитивно).
- baseline_price для OOS-подписки = 0 (колонка NOT NULL, 0 валиден).

## Бот
`doTrack`: если `!InStock` — апсертим товар (`in_stock=false`), создаём подписку
`trigger_type=back_in_stock` с baseline/first_seen = last-цена (для опоры
below/disc). Сообщение: «🚫 нет в наличии. Последняя цена: X ₽». Клавиатура — 3
стратегии как у обычного товара, но вместо «любое снижение» — «🔔 Когда появится
в наличии» (помечена ✅): `[🔔 В наличии]` `[📉 Ниже цены] [％ Скидка %]`.
below/disc показываем только при известной last-цене (иначе процент считать не
от чего). Переиспользуют существующий FSM ввода суммы.

`back_in_stock` добавлен в `TriggerType.Valid`, `TriggerDescription`, и в callback
`ptrack:<id>:stock`.

## Не покрыто / TODO
- OOS-детект только для Я.Маркета (WB/Ozon всегда in stock).
- Алерт «снова в наличии» — отдельный текст (флаг `BackInStock` в `PriceAlert`).
- Деплой: контракт `PriceEvent` аддитивный → порядок выката scraper/notifier
  некритичен (безопасные дефолты).
