# migrations/ — схема БД

Инструмент — [goose](https://github.com/pressly/goose). Каждый файл содержит
секции `-- +goose Up` / `-- +goose Down`.

## Локально (чистая база)

```bash
make migrate            # goose -dir ./migrations postgres "$DB_URL" up
make migrate-status
```

Нумерация НЕ сплошная: `030` и `031` отсутствуют намеренно (см. ниже). Goose
непрерывности не требует.

## На проде

goose не используется: PG-порт наружу закрыт, а образ goose из ghcr недоступен
(см. `docs/SECURITY-HARDENING.md`). Миграции катаются руками:

```bash
docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1 \
  < migrations/0NN_name.sql
```

⚠️ **Грабля:** psql выполнит файл ЦЕЛИКОМ, включая секцию `Down`. Для миграции,
где `Down` дропает таблицу, это означает «создать и тут же удалить, без единой
ошибки» (поймано на `032`). Берите только `Up`:

```bash
awk '/-- \+goose Up/{f=1} /-- \+goose Down/{f=0} f' migrations/032_price_insight.sql \
  | grep -v 'goose Statement' \
  | docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1
```

Права на новые таблицы роль `tryberry_app` получает сама — через
`ALTER DEFAULT PRIVILEGES FOR ROLE "user"` из `scripts/pg-create-roles.sh`.

## Почему нет 030 и 031

Это были не миграции схемы, а **одноразовые скрипты починки данных**
(склейка дублей товаров и починка `products.marketplace`). Они написаны на
чистом psql с метакомандами `\echo` и без goose-аннотаций, поэтому `make migrate`
на них падал — то есть поднять базу с нуля по инструкции было нельзя.

Скрипты переехали к остальным разовым в `scripts/sql/`:

- `scripts/sql/030_product_marketplace_and_dupes.sql`
- `scripts/sql/031_yandex_market_dupes.sql`

На чистой базе они не нужны: дублей и кривых ярлыков там нет, а канон URL
пишет сам код (`internal/scraper/canonical_url.go`). На проде оба уже применены
(16.07.2026). Обе идемпотентны — повторный прогон ничего не найдёт.

## Порядок при выкатке миграций, меняющих ключ товара

`031` содержит правило, которое стоило 96 кривых строк: **сначала код, потом
миграция**. Выдача активно апсертит товары, и старый код, не знающий канона,
заведёт их заново со слагами в окне между миграцией и деплоем.
