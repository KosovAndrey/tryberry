-- +goose Up
-- +goose StatementBegin

-- Погашение промокода привязывается к ИДЕНТИЧНОСТИ (telegram_id / vk_id), а не
-- к строке users: как и с trial_claims (014), отвязка платформы пересоздавала
-- строку, и UNIQUE(code_id, user_id) в promo_redemptions позволял погасить тот
-- же код повторно. Записи вечные, переживают отвязки и слияния.
CREATE TABLE IF NOT EXISTS promo_claims (
    code_id     BIGINT      NOT NULL REFERENCES promo_codes(id),
    platform    TEXT        NOT NULL CHECK (platform IN ('tg', 'vk')),
    external_id BIGINT      NOT NULL,
    claimed_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (code_id, platform, external_id)
);

-- Бэкфилл по текущим идентичностям погасивших (по уже отвязанным восстановить
-- нечего — их идентичности больше не знаем).
INSERT INTO promo_claims (code_id, platform, external_id)
SELECT pr.code_id, 'tg', u.telegram_id
FROM promo_redemptions pr JOIN users u ON u.id = pr.user_id
WHERE u.telegram_id IS NOT NULL
ON CONFLICT DO NOTHING;
INSERT INTO promo_claims (code_id, platform, external_id)
SELECT pr.code_id, 'vk', u.vk_id
FROM promo_redemptions pr JOIN users u ON u.id = pr.user_id
WHERE u.vk_id IS NOT NULL
ON CONFLICT DO NOTHING;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS promo_claims;
-- +goose StatementEnd
