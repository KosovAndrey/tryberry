-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 027_trial_winback.sql — win-back конца триала (docs/TARIFF-FREE-SEARCH-LINK.md):
-- одна строка на триальщика = его персональный discount-код и отметки трёх
-- стадий пушей. Идемпотентность рассылки держится на stageN_sent_at
-- (NULL → стадия ещё не отправлена; повтор при сбое отправки — следующим тиком).
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS trial_winbacks (
    user_id         BIGINT PRIMARY KEY REFERENCES users(id),
    promo_code_id   BIGINT NOT NULL REFERENCES promo_codes(id),
    code            TEXT NOT NULL,
    code_expires_at TIMESTAMPTZ NOT NULL,
    stage1_sent_at  TIMESTAMPTZ,
    stage2_sent_at  TIMESTAMPTZ,
    stage3_sent_at  TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS trial_winbacks;

-- +goose StatementEnd
