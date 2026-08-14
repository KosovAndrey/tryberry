# TryBerry

Бот отслеживания цен на российских маркетплейсах: **Wildberries, Ozon,
Яндекс.Маркет, AliExpress**. Работает в Telegram, VK и MAX (полный паритет
функций между каналами). Сайт — [tryberry.ru](https://tryberry.ru).

Что умеет:

- **Трек товара** по ссылке — уведомление при снижении цены. Триггеры:
  любое снижение, цена ниже порога, скидка от N%, «снова в наличии».
- **Трек поисковой выдачи** — подписка не на товар, а на весь запрос или на
  витрину продавца: бот следит за всеми позициями сразу.
- **«Честная цена»** — вердикт по собственной истории наблюдений (минимум за
  30/90 дней / всё время, медиана, взвешенная по времени). Нужен, чтобы
  отличать реальную скидку от «−40%» от задранной планки.
- **Публичный график цены** — страница `/p/<id>` с SSR и историей.
- Тарифы, промокоды, рефералы, автоплатежи (Робокасса, чеки НПД).

## Архитектура

```
                    ┌──────────┐
 TG / VK / MAX ───► │   api    │ ── telegram-updates ──► bot-worker ──► ответы
 вебхуки платёжек   │(ingestor)│ ── payments ──────────►
                    └──────────┘
                                                          ┌──► scrape-tasks ──────► scraper ×3 ──┐
                    ┌───────────┐                         │                                      │
                    │ scheduler │ ── due-based эмиссия ────┼──► ozon-scrape-tasks ──► scraper ────┤
                    │(синглтон) │                          ├──► search-tasks ──► search-worker ───┤
                    └───────────┘                          └──► reseller-tasks ─► reseller-worker ┤
                                                                                                  │
                            price-events / search-events ◄────────────────────────────────────────┘
                                     │
                        ┌────────────┴────────────┐
                        ▼                         ▼
                   notifier (Go)            price-insight (JVM)
              триггеры → outbox →           Kafka Streams: сегментное
              флашер → TG/VK/MAX            состояние цены → вердикт
```

12 сервисов в docker compose плюс инфраструктура: Postgres 16 (история цен
партиционирована по месяцам), Kafka, Redis, nginx+TLS, Prometheus/Grafana/
Alertmanager/Loki/Jaeger, xray (VLESS-egress к Telegram с RU-хостинга).

Отдельный слой — **Python-сайдкары с браузером**: `ozon-miner`,
`wb-search-miner`, `ali-miner`. Они существуют потому, что антибот площадок
(Ozon FAB, wbaas, AliExpress X5SEC) проверяет TLS-отпечаток и репутацию IP,
и часть запросов проходит только из живого прогретого браузера.

**Языки:** Go (7 сервисов, ~42 тыс. строк), Python (сайдкары), Java 21 +
Spring Boot + Kafka Streams (`price-insight`).

## Документация

Начинать с [`docs/PROJECT-MAP.md`](docs/PROJECT-MAP.md) — карта решений с
причинами и ценой. Индекс остальных доков со статусами —
[`docs/README.md`](docs/README.md).

Правило проекта: **у каждой темы один канон**; если код и док расходятся —
прав код, а док надо чинить.

## Локальный запуск

Нужны Docker и Go 1.26+ (`~/.local/go`, системного нет).

```bash
cp .env.example .env      # заполнить TELEGRAM_BOT_TOKEN и пароли
make up                   # поднять всё
make migrate              # накатить схему (goose); см. migrations/README.md
```

Проверки:

```bash
make test                 # go test -race ./...
make lint                 # golangci-lint (11 линтеров)
cd price-insight && mvn test
```

## Прод

Деплой — вручную с сервера, всегда **двумя** compose-файлами:

```bash
make deploy               # build + up -d + nginx reload + check-ports
```

`make deploy` защищён `guard-clean-shell` (экспортированные в шелл переменные
перебивают `.env` при интерполяции compose — на эту граблю наступали дважды) и
завершается `check-ports` (наружу разрешены только 80/443 nginx).

Подробности: [`README-deploy.md`](README-deploy.md),
[`docs/SECURITY-HARDENING.md`](docs/SECURITY-HARDENING.md).

## Состояние

Первые живые пользователи — с 14.08.2026. Один разработчик.
Известные пробелы (CI, покрытие SQL тестами, единственный брокер Kafka)
перечислены честно в [`docs/PROJECT-MAP.md`](docs/PROJECT-MAP.md) §7.
