-- +goose Up
-- +goose StatementBegin

-- Робокасса + подписки. Расширяем payments под мульти-провайдер и рекуррент.
-- provider — какой шлюз обработал платёж (флаг PAYMENT_PROVIDER на момент create).
-- kind — onetime | subscription_initial | subscription_renewal.
-- Для Робокассы внешний id платежа == payments.id (InvId числовой), yk_payment_id
-- остаётся только для ЮKassa и потому nullable.
ALTER TABLE payments
    ADD COLUMN IF NOT EXISTS provider TEXT NOT NULL DEFAULT 'yookassa',
    ADD COLUMN IF NOT EXISTS kind     TEXT NOT NULL DEFAULT 'onetime'
                             CHECK (kind IN ('onetime', 'subscription_initial', 'subscription_renewal')),
    ADD COLUMN IF NOT EXISTS billing_subscription_id BIGINT;

-- Биллинговые (рекуррентные) подписки. НЕ путать с таблицей subscriptions —
-- та про товарные подписки на снижение цены. Одна активная подписка на юзера.
CREATE TABLE IF NOT EXISTS billing_subscriptions (
    id                   BIGSERIAL   PRIMARY KEY,
    user_id              BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan                 TEXT        NOT NULL,
    status               TEXT        NOT NULL DEFAULT 'active'
                                     CHECK (status IN ('active', 'canceled', 'past_due', 'expired')),
    amount_kopecks       BIGINT      NOT NULL,                 -- сумма автопродления (sub-цена, без промо)
    recurring_invoice_id BIGINT      NOT NULL,                 -- PreviousInvoiceID = InvId первого платежа
    next_charge_at       TIMESTAMPTZ NOT NULL,                 -- когда списывать следующее продление
    pre_notice_sent_at   TIMESTAMPTZ,                          -- когда отправили предупреждение о списании
    fail_count           INT         NOT NULL DEFAULT 0,       -- подряд неудачных списаний (dunning)
    last_payment_id      BIGINT,                               -- последний payments.id по подписке
    created_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at           TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    canceled_at          TIMESTAMPTZ
);

-- Одна активная подписка на пользователя (partial unique).
CREATE UNIQUE INDEX IF NOT EXISTS uq_billing_sub_active
    ON billing_subscriptions(user_id) WHERE status = 'active';

-- Шедулер автосписаний выбирает по (status, next_charge_at).
CREATE INDEX IF NOT EXISTS idx_billing_sub_due
    ON billing_subscriptions(status, next_charge_at);

-- FK payments → billing_subscriptions добавляем после создания таблицы.
ALTER TABLE payments
    ADD CONSTRAINT fk_payments_billing_sub
    FOREIGN KEY (billing_subscription_id) REFERENCES billing_subscriptions(id) ON DELETE SET NULL;

-- Лог явного согласия на подписку (защита от чарджбэка и претензий по ЗоЗПП).
CREATE TABLE IF NOT EXISTS subscription_consents (
    id             BIGSERIAL   PRIMARY KEY,
    user_id        BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    plan           TEXT        NOT NULL,
    amount_kopecks BIGINT      NOT NULL,
    terms_version  TEXT        NOT NULL,
    platform       TEXT        NOT NULL,                       -- tg | vk
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_sub_consents_user ON subscription_consents(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS subscription_consents;
ALTER TABLE payments DROP CONSTRAINT IF EXISTS fk_payments_billing_sub;
DROP TABLE IF EXISTS billing_subscriptions;
ALTER TABLE payments
    DROP COLUMN IF EXISTS billing_subscription_id,
    DROP COLUMN IF EXISTS kind,
    DROP COLUMN IF EXISTS provider;
-- +goose StatementEnd
