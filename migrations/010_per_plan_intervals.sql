-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 010_per_plan_intervals.sql — строгий per-plan интервал проверки для ТОВАРНОГО
-- пути (поисковый уже покрыт миграцией 009) + миграция имён планов.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- products.last_enqueued_at — отметка планировщика «товар поставлен в очередь»
-- (due-based: товар скрейпится не чаще MIN-интервала своих подписчиков).
-- subscriptions.last_evaluated_at — отметка notifier «подписка оценена»; throttle:
-- Free (60 мин) не получает уведомления чаще своего плана, даже если товар делит
-- с Pro (15 мин) и физически скрейпится каждые 15 минут.

ALTER TABLE products
    ADD COLUMN IF NOT EXISTS last_enqueued_at TIMESTAMPTZ;

ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS last_evaluated_at TIMESTAMPTZ;

-- Миграция имён планов под новый каталог (basic/старый reseller удаляются из
-- продажи; legacy-алиасы в коде ещё их понимают, но переводим на канонические).
UPDATE users SET plan = 'pro'           WHERE plan = 'basic';
UPDATE users SET plan = 'reseller_pro'  WHERE plan = 'reseller';

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

ALTER TABLE subscriptions DROP COLUMN IF EXISTS last_evaluated_at;
ALTER TABLE products DROP COLUMN IF EXISTS last_enqueued_at;

-- +goose StatementEnd
