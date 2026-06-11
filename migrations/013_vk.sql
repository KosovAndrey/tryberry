-- +goose Up
-- +goose StatementBegin

-- VK-интеграция: один юзер может иметь обе идентичности (TG и/или VK).
-- telegram_id становится nullable — юзер, пришедший из VK, живёт без него,
-- но хотя бы одна идентичность обязана быть (CHECK).
ALTER TABLE users ALTER COLUMN telegram_id DROP NOT NULL;
ALTER TABLE users ADD COLUMN IF NOT EXISTS vk_id BIGINT UNIQUE;
ALTER TABLE users ADD COLUMN IF NOT EXISTS notify_channel TEXT NOT NULL DEFAULT 'auto';

ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_has_identity;
ALTER TABLE users ADD CONSTRAINT chk_user_has_identity
    CHECK (telegram_id IS NOT NULL OR vk_id IS NOT NULL);

ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_notify_channel;
ALTER TABLE users ADD CONSTRAINT chk_user_notify_channel
    CHECK (notify_channel IN ('auto', 'tg', 'vk', 'both'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_notify_channel;
ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_has_identity;
ALTER TABLE users DROP COLUMN IF EXISTS notify_channel;
ALTER TABLE users DROP COLUMN IF EXISTS vk_id;
-- ВНИМАНИЕ: NOT NULL обратно не возвращаем — могли появиться VK-only юзеры.
-- +goose StatementEnd
