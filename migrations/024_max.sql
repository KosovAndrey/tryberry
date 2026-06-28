-- +goose Up
-- +goose StatementBegin

-- MAX-мессенджер: третья идентичность наравне с Telegram и VK. Один юзер может
-- иметь любую комбинацию (tg / vk / max), хотя бы одна обязательна (CHECK).
-- max_id = schemes.User.UserId официального Bot API MAX.
ALTER TABLE users ADD COLUMN IF NOT EXISTS max_id BIGINT UNIQUE;

ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_has_identity;
ALTER TABLE users ADD CONSTRAINT chk_user_has_identity
    CHECK (telegram_id IS NOT NULL OR vk_id IS NOT NULL OR max_id IS NOT NULL);

-- notify_channel получает 'max' (конкретный канал) и 'all' (все привязанные).
-- 'both' (легаси tg+vk) сохраняем для совместимости.
ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_notify_channel;
ALTER TABLE users ADD CONSTRAINT chk_user_notify_channel
    CHECK (notify_channel IN ('auto', 'tg', 'vk', 'max', 'both', 'all'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_notify_channel;
ALTER TABLE users ADD CONSTRAINT chk_user_notify_channel
    CHECK (notify_channel IN ('auto', 'tg', 'vk', 'both'));

ALTER TABLE users DROP CONSTRAINT IF EXISTS chk_user_has_identity;
ALTER TABLE users ADD CONSTRAINT chk_user_has_identity
    CHECK (telegram_id IS NOT NULL OR vk_id IS NOT NULL);

ALTER TABLE users DROP COLUMN IF EXISTS max_id;
-- +goose StatementEnd
