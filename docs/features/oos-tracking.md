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
`scraper.Result.InStock bool`. Скрейпер с ценой → `InStock=true`. Я.Маркет: если
в JSON-LD есть узел `Product`, но цена не найдена ни в JSON-LD, ни в стейте →
`Result{Price:0, InStock:false}` (УСПЕХ, не ошибка). WB/Ozon всегда `InStock=true`.

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
с `trigger_type=back_in_stock`, baseline 0, показываем OOS-сообщение и клавиатуру:
«🔔 Когда появится в наличии» (back_in_stock, помечена) + «📉 Когда цена ниже…»
(переиспользует существующий FSM ввода суммы `below_target`).

`back_in_stock` добавлен в `TriggerType.Valid`, `TriggerDescription`, и в callback
`ptrack:<id>:stock`.

## Не покрыто / TODO
- OOS-детект только для Я.Маркета (WB/Ozon всегда in stock).
- Алерт «снова в наличии» — отдельный текст (флаг `BackInStock` в `PriceAlert`).
- Деплой: контракт `PriceEvent` аддитивный → порядок выката scraper/notifier
  некритичен (безопасные дефолты).
