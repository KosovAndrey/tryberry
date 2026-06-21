-- +goose Up
-- +goose StatementBegin

-- last_digest_at — когда юзеру последний раз слали (или оценивали) персональный
-- дайджест «твои товары сейчас». NULL = ни разу. Дайджест еженедельный: notifier
-- берёт юзеров с last_digest_at IS NULL OR < now()-интервал и активными подписками.
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_digest_at TIMESTAMPTZ;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN IF EXISTS last_digest_at;
-- +goose StatementEnd
