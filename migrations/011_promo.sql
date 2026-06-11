-- +goose Up
-- +goose StatementBegin

-- Промокоды. Два типа эффекта:
--   grant    — N дней тарифа X бесплатно (работает без оплаты);
--   discount — скидка % к платежу (применяется при оплате через ЮKassa,
--              погашается только после успешного платежа).
CREATE TABLE IF NOT EXISTS promo_codes (
    id           BIGSERIAL PRIMARY KEY,
    code         TEXT UNIQUE NOT NULL,          -- хранится в UPPER
    kind         TEXT NOT NULL,                 -- 'grant' | 'discount'
    plan         TEXT,                          -- grant: какой план
    days         INT,                           -- grant: на сколько дней
    discount_pct SMALLINT,                      -- discount: процент скидки
    max_uses     INT NOT NULL,
    used_count   INT NOT NULL DEFAULT 0,
    expires_at   TIMESTAMPTZ,                   -- NULL = бессрочный код
    active       BOOLEAN NOT NULL DEFAULT TRUE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_promo_kind CHECK (kind IN ('grant', 'discount')),
    CONSTRAINT chk_promo_grant_fields
        CHECK (kind <> 'grant' OR (plan IS NOT NULL AND days IS NOT NULL AND days > 0)),
    CONSTRAINT chk_promo_discount_fields
        CHECK (kind <> 'discount' OR (discount_pct IS NOT NULL AND discount_pct BETWEEN 1 AND 99)),
    CONSTRAINT chk_promo_max_uses CHECK (max_uses > 0)
);

-- Погашения: «один код один раз на юзера» обеспечивает UNIQUE, а не код приложения.
CREATE TABLE IF NOT EXISTS promo_redemptions (
    id          BIGSERIAL PRIMARY KEY,
    code_id     BIGINT NOT NULL REFERENCES promo_codes(id),
    user_id     BIGINT NOT NULL REFERENCES users(id),
    redeemed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (code_id, user_id)
);

CREATE INDEX IF NOT EXISTS idx_promo_redemptions_user ON promo_redemptions (user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS promo_redemptions;
DROP TABLE IF EXISTS promo_codes;
-- +goose StatementEnd
