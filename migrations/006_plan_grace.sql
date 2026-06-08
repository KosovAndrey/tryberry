-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 006_plan_grace.sql — grace-период для поиск-подписок после истечения плана.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Когда план (триал/pro) истекает, поиск-подписки сверх лимита free НЕ удаляются
-- сразу, а ставятся на ПАУЗУ. Reconciler в notifier периодически:
--   1) ставит на паузу активные поиски юзеров с истёкшим планом,
--   2) восстанавливает их, если юзер вернул план в течение grace-окна,
--   3) удаляет паузные старше grace (каскад чистит baseline и историю).
--
-- Семантика inactive-строк (active = FALSE):
--   paused_at IS NULL     → удалена пользователем вручную (reconciler НЕ трогает)
--   paused_at IS NOT NULL → пауза по истечению плана (восстановима внутри grace)

ALTER TABLE search_subscriptions
    ADD COLUMN IF NOT EXISTS paused_at TIMESTAMPTZ;

-- Reconciler ищет паузные строки (восстановление/очистка) — частичный индекс.
CREATE INDEX IF NOT EXISTS idx_search_subs_paused
    ON search_subscriptions (paused_at) WHERE paused_at IS NOT NULL;

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_search_subs_paused;
ALTER TABLE search_subscriptions DROP COLUMN IF EXISTS paused_at;

-- +goose StatementEnd
