-- +goose Up
-- +goose StatementBegin

-- pending_alerts — durable outbox доставки уведомлений. Консьюмер price-events
-- решает (throttle+триггер) и кладёт сюда строку, двигая стейт подписки вперёд
-- (baseline/notified) НА РЕШЕНИИ; доставку гарантирует отдельный флашер ретраями
-- (at-least-once). Так горячий путь не блокируется медленной Telegram-отправкой,
-- и lag по price-events не копится. См. docs/SCALING-NOTIFIER-DELIVERY.md.
CREATE TABLE IF NOT EXISTS pending_alerts (
    id              BIGSERIAL   PRIMARY KEY,
    user_id         BIGINT      NOT NULL,
    subscription_id BIGINT      NOT NULL,
    product_id      BIGINT      NOT NULL,
    -- payload — JSON telegram.PriceAlert: всё для рендера (имя/url/old/new/honest/
    -- back_in_stock + роутинг ChatID/UserID). Канал (TG/VK) флашер резолвит через
    -- deliverer по users.notify_channel, поэтому отдельной колонки канала нет.
    payload         JSONB       NOT NULL,
    -- idem_key — тот же ключ идемпотентности, что в notifications
    -- (sub:price:recordedAt). UNIQUE гасит задвоение при перечитывании Kafka.
    idem_key        TEXT        NOT NULL,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- deliver_after — когда можно слать (бэкофф при сбое; окно бандлинга в Phase 1b).
    deliver_after   TIMESTAMPTZ NOT NULL DEFAULT now(),
    sent_at         TIMESTAMPTZ,            -- NULL = ещё не доставлено
    attempts        INT         NOT NULL DEFAULT 0,
    last_error      TEXT
);

CREATE UNIQUE INDEX IF NOT EXISTS pending_alerts_idem_key_uidx
    ON pending_alerts (idem_key);

-- Горячая выборка флашера: недоставленные, созревшие — по очереди.
CREATE INDEX IF NOT EXISTS pending_alerts_due_idx
    ON pending_alerts (deliver_after)
    WHERE sent_at IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS pending_alerts;
-- +goose StatementEnd
