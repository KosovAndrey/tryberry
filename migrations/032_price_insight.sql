-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 032_price_insight.sql — материализованный вердикт «честной цены».
-- Пишет price-insight (JVM, Kafka Streams), читает notifier вместо тяжёлой
-- агрегации по price_history на горячем пути. См. docs/PRICE-INSIGHT-JAVA.md.
--
-- Таблица ПРОИЗВОДНАЯ: источник правды остаётся в price_history. Её можно
-- в любой момент очистить и пересобрать проигрыванием price-events с начала,
-- поэтому ни внешних ключей, ни ON DELETE CASCADE тут нет — лишние связи
-- только помешают пересборке.
-- ─────────────────────────────────────────────────────────────────────────────

CREATE TABLE IF NOT EXISTS price_insight (
    product_id     BIGINT      PRIMARY KEY,
    -- verdict — код domain.PriceVerdict (iota из honest_price.go):
    -- 0 insufficient, 1 lowest_ever, 2 lowest_90, 3 lowest_30, 4 typical,
    -- 5 above_typical. Порядок менять нельзя, только дописывать в конец.
    verdict        SMALLINT    NOT NULL,
    min_30         NUMERIC(12, 2),
    median_30      NUMERIC(12, 2),
    min_90         NUMERIC(12, 2),
    min_all        NUMERIC(12, 2),
    -- observed_since — когда МЫ начали наблюдать товар. Нужен читателю, чтобы
    -- отличить «данных мало» от «данных нет».
    observed_since TIMESTAMPTZ,
    -- computed_at — момент stream-time, на который посчитан вердикт. По нему
    -- notifier решает, свежий ли он; протух — откатывается на PriceHistoryRepo.Stats.
    computed_at    TIMESTAMPTZ NOT NULL
);

-- Отбор протухших строк при выборе стратегии чтения.
CREATE INDEX IF NOT EXISTS price_insight_computed_at_idx
    ON price_insight (computed_at);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP TABLE IF EXISTS price_insight;
-- +goose StatementEnd
