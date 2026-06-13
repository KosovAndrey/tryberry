-- +goose Up
-- +goose StatementBegin

-- Email покупателя для фискального чека 54-ФЗ (самозанятость): ЮKassa отправляет
-- чек на этот адрес. Спрашиваем один раз перед первой оплатой, переиспользуем.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE users DROP COLUMN IF EXISTS email;
-- +goose StatementEnd
