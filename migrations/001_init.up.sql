-- +goose Up
-- +goose StatementBegin

-- Users
CREATE TABLE IF NOT EXISTS users (
                                     id          BIGSERIAL PRIMARY KEY,
                                     telegram_id BIGINT UNIQUE NOT NULL,
                                     username    TEXT,
                                     created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
    );

-- Products
CREATE TABLE IF NOT EXISTS products (
                                        id          BIGSERIAL PRIMARY KEY,
                                        url         TEXT UNIQUE NOT NULL,
                                        name        TEXT NOT NULL,
                                        image_url   TEXT,
                                        created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
    );

-- Price history (партиционирована по месяцам)
-- Новые партиции создаются автоматически через partition.Manager
CREATE TABLE IF NOT EXISTS price_history (
                                             id          BIGSERIAL,
                                             product_id  BIGINT NOT NULL REFERENCES products(id),
    price       NUMERIC(12, 2) NOT NULL,
    recorded_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
    ) PARTITION BY RANGE (recorded_at);

-- Стартовые партиции: текущий месяц + следующие два
CREATE TABLE IF NOT EXISTS price_history_2026_05 PARTITION OF price_history
    FOR VALUES FROM ('2026-05-01') TO ('2026-06-01');

CREATE TABLE IF NOT EXISTS price_history_2026_06 PARTITION OF price_history
    FOR VALUES FROM ('2026-06-01') TO ('2026-07-01');

CREATE TABLE IF NOT EXISTS price_history_2026_07 PARTITION OF price_history
    FOR VALUES FROM ('2026-07-01') TO ('2026-08-01');

-- Subscriptions
CREATE TABLE IF NOT EXISTS subscriptions (
                                             id             BIGSERIAL PRIMARY KEY,
                                             user_id        BIGINT NOT NULL REFERENCES users(id),
    product_id     BIGINT NOT NULL REFERENCES products(id),
    baseline_price NUMERIC(12, 2) NOT NULL,
    active         BOOLEAN NOT NULL DEFAULT TRUE,
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    UNIQUE (user_id, product_id)
    );

-- Notifications (idempotency)
CREATE TABLE IF NOT EXISTS notifications (
                                             id              BIGSERIAL PRIMARY KEY,
                                             subscription_id BIGINT NOT NULL REFERENCES subscriptions(id),
    old_price       NUMERIC(12, 2) NOT NULL,
    new_price       NUMERIC(12, 2) NOT NULL,
    sent_at         TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    idempotency_key TEXT UNIQUE NOT NULL
    -- формат ключа: "{subscription_id}:{new_price}:{recorded_at}"
    );

-- Индексы
CREATE INDEX IF NOT EXISTS idx_subscriptions_product
    ON subscriptions (product_id) WHERE active = TRUE;

CREATE INDEX IF NOT EXISTS idx_subscriptions_user
    ON subscriptions (user_id) WHERE active = TRUE;

CREATE INDEX IF NOT EXISTS idx_price_history_product
    ON price_history (product_id, recorded_at DESC);

CREATE INDEX IF NOT EXISTS idx_notifications_key
    ON notifications (idempotency_key);

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS notifications;
DROP TABLE IF EXISTS subscriptions;

DROP TABLE IF EXISTS price_history_2026_05;
DROP TABLE IF EXISTS price_history_2026_06;
DROP TABLE IF EXISTS price_history_2026_07;
DROP TABLE IF EXISTS price_history;

DROP TABLE IF EXISTS products;
DROP TABLE IF EXISTS users;

-- +goose StatementEnd