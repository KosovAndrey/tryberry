-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 029_user_attribution.sql — атрибуция первого касания (docs/PROMO-SHORTS-PLAN.md §6):
-- какой ролик/формат/площадка привёл юзера в бота. Одна строка на юзера =
-- первое касание, write-once (PK + ON CONFLICT DO NOTHING в repo). Пишется при
-- первом /start с payload v_<формат>_<площадка> (TG ?start=, VK ?ref=, MAX payload),
-- в TG — только после согласия ПД (payload до согласия ждёт в Redis).
-- Не пересекается с promo_/ref_/link_ — у тех своя обработка.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS user_attribution (
    user_id    BIGINT PRIMARY KEY REFERENCES users(id) ON DELETE CASCADE,
    channel    TEXT NOT NULL CHECK (channel IN ('tg', 'vk', 'max')),
    payload    TEXT NOT NULL,  -- сырой payload как пришёл (v_f1_yt)
    format     TEXT NOT NULL,  -- формат ролика (f1..f5, но не валидируем — матрица растёт)
    platform   TEXT NOT NULL,  -- площадка (yt, vk, ig, tt, site, ...)
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_user_attribution_fmt_platform
    ON user_attribution(format, platform);

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS user_attribution;

-- +goose StatementEnd
