-- +goose Up
-- +goose StatementBegin

-- Наличие товара: источник истины для триггера back_in_stock. Обновляется
-- скрейпером на каждом скрейпе; notifier по переходу false→true шлёт уведомление
-- «снова в наличии». DEFAULT TRUE — все существующие товары считаем в наличии.
ALTER TABLE products ADD COLUMN IF NOT EXISTS in_stock BOOLEAN NOT NULL DEFAULT TRUE;

-- Новый тип триггера для товарных подписок: уведомить, когда товар снова появится
-- в наличии. Заводится на карточках, добавленных без активного оффера.
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_trigger_type;
ALTER TABLE subscriptions ADD  CONSTRAINT chk_sub_trigger_type
    CHECK (trigger_type IN ('below_target','any_drop','discount_pct','back_in_stock'));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_trigger_type;
ALTER TABLE subscriptions ADD  CONSTRAINT chk_sub_trigger_type
    CHECK (trigger_type IN ('below_target','any_drop','discount_pct'));
ALTER TABLE products DROP COLUMN IF EXISTS in_stock;
-- +goose StatementEnd
