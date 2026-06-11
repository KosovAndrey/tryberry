-- +goose Up
-- +goose StatementBegin

-- Кто привёл пользователя. Ставится один раз при первом /start ref_XXX
-- (только для свежесозданных аккаунтов) и никогда не перезаписывается.
ALTER TABLE users ADD COLUMN IF NOT EXISTS referred_by BIGINT REFERENCES users(id);

CREATE INDEX IF NOT EXISTS idx_users_referred_by ON users (referred_by) WHERE referred_by IS NOT NULL;

-- Аудит реферальных начислений. За одного приглашённого и одно событие —
-- ровно одна награда (UNIQUE), сколько бы раз reconciler ни проверял.
-- События: 'activated' — друг живёт 48ч и держит активную подписку;
--          'paid'      — друг оплатил тариф (замыкается на ЮKassa).
CREATE TABLE IF NOT EXISTS referral_rewards (
    id          BIGSERIAL PRIMARY KEY,
    referrer_id BIGINT NOT NULL REFERENCES users(id),
    referee_id  BIGINT NOT NULL REFERENCES users(id),
    event       TEXT NOT NULL,
    days        INT NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    CONSTRAINT chk_ref_event CHECK (event IN ('activated', 'paid')),
    UNIQUE (referee_id, event)
);

CREATE INDEX IF NOT EXISTS idx_referral_rewards_referrer ON referral_rewards (referrer_id, created_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS referral_rewards;
DROP INDEX IF EXISTS idx_users_referred_by;
ALTER TABLE users DROP COLUMN IF EXISTS referred_by;
-- +goose StatementEnd
