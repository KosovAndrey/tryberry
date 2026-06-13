-- +goose Up
-- +goose StatementBegin

-- Платежи ЮKassa. Одна строка на попытку оплаты. Идемпотентность вебхуков —
-- через UNIQUE(yk_payment_id): применяет план только первый переход
-- status pending→succeeded (UPDATE ... WHERE status='pending' RETURNING).
-- idempotence_key — наш ключ на create в ЮKassa (повтор POST не плодит платёж).
CREATE TABLE IF NOT EXISTS payments (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         BIGINT      NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    yk_payment_id   TEXT        UNIQUE,                      -- id платежа в ЮKassa
    idempotence_key TEXT        NOT NULL UNIQUE,             -- наш ключ на create
    plan            TEXT        NOT NULL,
    days            INT         NOT NULL DEFAULT 30,
    amount_kopecks  BIGINT      NOT NULL,                    -- сумма к оплате (со скидкой)
    promo_code_id   BIGINT      REFERENCES promo_codes(id),  -- discount-код, если применён
    status          TEXT        NOT NULL DEFAULT 'pending'   -- pending | succeeded | canceled
                                CHECK (status IN ('pending', 'succeeded', 'canceled')),
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    paid_at         TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_payments_user ON payments(user_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS payments;
-- +goose StatementEnd
