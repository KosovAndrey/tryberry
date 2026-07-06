-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 026_volatility_cadence.sql — данные для каданса по волатильности
-- (docs/TARIFF-FREE-SEARCH-LINK.md §2): планировщик замедляет опрос
-- товаров/выдач, чья цена давно непрерывно не меняется (бэкофф ×1..×3),
-- и мгновенно возвращает полный темп при первом изменении.
-- ─────────────────────────────────────────────────────────────────────────────
--
-- products.last_price_change_at — когда цена товара менялась в последний раз.
--   Проставляет scraper-worker там же, где делает change-only INSERT в
--   price_history. NULL = новый трек без истории (множитель ×1).
-- search_queries.last_change_at — когда менялась МИНИМАЛЬНАЯ цена топ-N выдачи
--   (состав/позиции изменением не считаются — ротация выдачи Ozon дребезжит).
-- search_queries.last_min_price — та самая минимальная цена (для сравнения на
--   следующем скрейпе; сравнение и апдейт — одним UPDATE в search-worker).

ALTER TABLE products
    ADD COLUMN IF NOT EXISTS last_price_change_at TIMESTAMPTZ;

ALTER TABLE search_queries
    ADD COLUMN IF NOT EXISTS last_change_at TIMESTAMPTZ,
    ADD COLUMN IF NOT EXISTS last_min_price NUMERIC(12, 2);

-- Бэкфилл: price_history уже change-only, значит возраст последнего изменения
-- цены = MAX(recorded_at). Для товаров с единственной точкой это момент начала
-- трекинга — семантика «нового трека» сохраняется (бэкофф начнётся не раньше
-- чем через 5 дней после неё).
UPDATE products p
SET last_price_change_at = ph.max_recorded
FROM (
    SELECT product_id, MAX(recorded_at) AS max_recorded
    FROM price_history
    GROUP BY product_id
) ph
WHERE p.id = ph.product_id
  AND p.last_price_change_at IS NULL;

-- search_queries бэкфиллить нечем (мин. цену топ-N раньше не хранили):
-- NULL = «новый трек», первый же скрейп проставит last_min_price/last_change_at.

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

ALTER TABLE search_queries
    DROP COLUMN IF EXISTS last_min_price,
    DROP COLUMN IF EXISTS last_change_at;

ALTER TABLE products DROP COLUMN IF EXISTS last_price_change_at;

-- +goose StatementEnd
