-- +goose Up
-- +goose StatementBegin

-- Триал привязывается к ИДЕНТИЧНОСТИ (telegram_id / vk_id), а не к строке users:
-- отвязка платформы пересоздаёт строку с trial_used=FALSE, что позволяло фармить
-- триалы циклом «отвязать → /start → триал». Запись здесь вечная и переживает
-- любые отвязки/слияния. Пишется при активации триала для всех идентичностей юзера.
CREATE TABLE IF NOT EXISTS trial_claims (
    platform    TEXT        NOT NULL CHECK (platform IN ('tg', 'vk')),
    external_id BIGINT      NOT NULL,
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (platform, external_id)
);

-- Бэкфилл по уже использованным триалам.
INSERT INTO trial_claims (platform, external_id)
SELECT 'tg', telegram_id FROM users WHERE trial_used AND telegram_id IS NOT NULL
ON CONFLICT DO NOTHING;
INSERT INTO trial_claims (platform, external_id)
SELECT 'vk', vk_id FROM users WHERE trial_used AND vk_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- Аудит слияний аккаунтов (разборы в поддержке: кто, что и во что слил).
-- Без FK: absorbed-строка удаляется в момент слияния, kept может быть удалена позже.
CREATE TABLE IF NOT EXISTS account_merges (
    id                  BIGSERIAL PRIMARY KEY,
    kept_user_id        BIGINT      NOT NULL,
    absorbed_user_id    BIGINT      NOT NULL,
    kept_plan           TEXT        NOT NULL,
    kept_expires_at     TIMESTAMPTZ,
    absorbed_plan       TEXT        NOT NULL,
    absorbed_expires_at TIMESTAMPTZ,
    result_plan         TEXT        NOT NULL,
    result_expires_at   TIMESTAMPTZ,
    detail              JSONB       NOT NULL DEFAULT '{}'::jsonb, -- идентичности, выбранный вариант, дни
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS account_merges;
DROP TABLE IF EXISTS trial_claims;
-- +goose StatementEnd
