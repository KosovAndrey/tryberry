-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 034_live_price_retry.sql — быстрый повтор скрейпа, когда живой цены не было.
--
-- С 08-10-2026 WB без живой карточки скрейп пропускает (архивную цену не
-- отдаём). Сайдкар wb-search-miner при этом 10–20% времени стоит без живых
-- дорожек — в основном короткими «волнами» 498 по всему IP на 30–60с. Раньше
-- такой товар ждал следующей плановой задачи (15–60 мин, с бэкоффом до 3ч),
-- хотя волна давно прошла. Теперь scraper назначает повтор через 2, 4, 8…
-- минут (потолок 30): delay = 2мин × 2^streak. Удвоение само гасит повторы
-- при долгой стене, не нагружая сайдкар.
--
-- live_retry_at   — когда повторить (NULL = нет); планировщик считает товар
--                   due, когда срок наступил, и обнуляет поле при постановке.
-- live_fail_streak — сколько пропусков подряд; сбрасывается успешным скрейпом.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE products ADD COLUMN IF NOT EXISTS live_retry_at TIMESTAMPTZ;
ALTER TABLE products ADD COLUMN IF NOT EXISTS live_fail_streak INT NOT NULL DEFAULT 0;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE products DROP COLUMN IF EXISTS live_fail_streak;
ALTER TABLE products DROP COLUMN IF EXISTS live_retry_at;
-- +goose StatementEnd
