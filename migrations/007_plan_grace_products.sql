-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 007_plan_grace_products.sql — grace-период для ТОВАРНЫХ подписок.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Тот же механизм, что и для поиска (006), но для subscriptions. Отличие: у free
-- MaxProduct=10 (а не 0), поэтому при истечении плана reconciler гасит лишь
-- ИЗБЫТОК сверх 10 (старые подписки остаются), а не все.
--
-- Семантика inactive-строк (active = FALSE):
--   paused_at IS NULL     → удалена пользователем вручную (reconciler НЕ трогает)
--   paused_at IS NOT NULL → пауза по истечению плана (восстановима внутри grace)

ALTER TABLE subscriptions
    ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_subs_paused
    ON subscriptions (paused_at) WHERE paused_at IS NOT NULL;

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_subs_paused;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS paused_at;

-- +goose StatementEnd
