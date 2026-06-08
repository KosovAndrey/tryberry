-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 008_plan_reminded.sql — отметка об отправленном напоминании об истечении плана.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Reconciler в notifier шлёт разовое напоминание за сутки до конца тарифа.
-- plan_reminded_at защищает от повторной отправки на каждом тике. Сбрасывается
-- в NULL при смене плана (SetPlan/ActivateTrial) → новый срок = новое право на
-- напоминание.

ALTER TABLE users
    ADD COLUMN IF NOT EXISTS plan_reminded_at TIMESTAMPTZ;

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

ALTER TABLE users DROP COLUMN IF EXISTS plan_reminded_at;

-- +goose StatementEnd
