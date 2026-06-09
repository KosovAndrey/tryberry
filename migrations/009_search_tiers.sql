-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 009_search_tiers.sql — поддержка тарифа «перекуп» (частая выдача) + per-sub
-- throttle уведомлений.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Два независимых «интервала» (значения живут в коде, domain.Plans):
--   • интервал ЗАПРОСА  = MIN(интервал подписчиков) → как часто скрейпим выдачу
--     и в какую дорожку роутим (reseller-tasks / search-tasks).
--   • интервал ПОДПИСКИ = SearchInterval плана подписчика → как часто оцениваем
--     триггеры и шлём уведомления конкретному юзеру.
--
-- last_enqueued_at — отметка планировщика «запрос поставлен в очередь Kafka».
-- Решение «пора ли скрейпить снова» принимается по ней (а не по last_scraped_at,
-- который воркер пишет только при успехе): так провал скрейпа переэмитится через
-- свой интервал, а не блокируется на полный цикл. last_scraped_at остаётся за
-- воркером (успех) — для свежести/метрик.
--
-- last_evaluated_at — отметка воркера «подписка оценена». Throttle: перекуп
-- (interval=1м) оценивается каждый скрейп, обычный сосед того же запроса (дефолт)
-- — не чаще своего интервала, хотя выдача физически скрейпится раз в минуту.

ALTER TABLE search_queries
    ADD COLUMN IF NOT EXISTS last_enqueued_at TIMESTAMPTZ;

ALTER TABLE search_subscriptions
    ADD COLUMN IF NOT EXISTS last_evaluated_at TIMESTAMPTZ;

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

ALTER TABLE search_subscriptions DROP COLUMN IF EXISTS last_evaluated_at;
ALTER TABLE search_queries DROP COLUMN IF EXISTS last_enqueued_at;

-- +goose StatementEnd
