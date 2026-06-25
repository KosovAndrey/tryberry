-- +goose Up
-- +goose StatementBegin

-- public_id — стабильный непривязанный к имени токен товара для ПУБЛИЧНОЙ
-- веб-страницы графика цены (/p/<public_id>). Сырой products.id в URL давал бы
-- перебор всего каталога, поэтому отдаём 12-символьный hex из gen_random_uuid()
-- (в ядре PG13+, расширения не нужны). DEFAULT на уровне БД ⇒ горячий путь
-- ProductRepo.UpsertBatch не трогаем: INSERT не перечисляет public_id, default
-- срабатывает только на вставке и не меняется на ON CONFLICT.
ALTER TABLE products ADD COLUMN IF NOT EXISTS public_id TEXT;

UPDATE products
SET public_id = substr(translate(gen_random_uuid()::text, '-', ''), 1, 12)
WHERE public_id IS NULL;

ALTER TABLE products
    ALTER COLUMN public_id SET DEFAULT substr(translate(gen_random_uuid()::text, '-', ''), 1, 12);

ALTER TABLE products ALTER COLUMN public_id SET NOT NULL;

CREATE UNIQUE INDEX IF NOT EXISTS idx_products_public_id ON products (public_id);

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_products_public_id;
ALTER TABLE products DROP COLUMN IF EXISTS public_id;
-- +goose StatementEnd
