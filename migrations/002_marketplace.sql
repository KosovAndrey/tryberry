-- +goose Up
-- +goose StatementBegin

-- Добавляем колонку marketplace в products
ALTER TABLE products ADD COLUMN IF NOT EXISTS marketplace TEXT NOT NULL DEFAULT 'wildberries';

-- Убираем default чтобы новые записи требовали явного указания
ALTER TABLE products ALTER COLUMN marketplace DROP DEFAULT;

-- Индекс для группировки по маркетплейсу (для метрик и фильтров)
CREATE INDEX IF NOT EXISTS idx_products_marketplace ON products (marketplace);

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP INDEX IF EXISTS idx_products_marketplace;
ALTER TABLE products DROP COLUMN IF EXISTS marketplace;

-- +goose StatementEnd