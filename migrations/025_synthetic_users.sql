-- +goose Up
-- +goose StatementBegin

-- Синтетические юзеры нагрузочного теста (cmd/seed-loadtest). Живут в общей
-- таблице, чтобы конвейер scheduler→scraper→notifier видел их как настоящих;
-- notifier по флагу редиректит/дропает доставку (см. docs/LOAD-TEST-SYNTHETIC.md).
-- Уборка: seed-loadtest cleanup удаляет юзеров и их подписки, товары и
-- price_history остаются (это и есть накопленные данные для графиков).
ALTER TABLE users ADD COLUMN IF NOT EXISTS is_synthetic BOOLEAN NOT NULL DEFAULT FALSE;

-- Частичный индекс: выборки cleanup/статистики; на реальных юзерах места не ест.
CREATE INDEX IF NOT EXISTS idx_users_synthetic ON users (id) WHERE is_synthetic;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_users_synthetic;
ALTER TABLE users DROP COLUMN IF EXISTS is_synthetic;
-- +goose StatementEnd
