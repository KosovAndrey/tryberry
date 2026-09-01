-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 033_ym_display_url.sql — ссылка для показа отдельно от ключа товара.
--
-- Зачем. 01-09-2026 Я.Маркет закрыл SmartCaptcha формы /product/<modelId> и
-- /product--<slug>/<modelId> и оставил живой только /card/<slug>/<oskuId>
-- (docs/YANDEX-CARD-MIGRATION.md). Слаг в ней декоративен — /card/x/<oskuId>
-- отдаёт ту же карточку, — но ЖИВОЙ: он меняется при переименовании товара. А
-- products.url это UNIQUE-ключ, по которому апсертятся товары: держи мы слаг в
-- ключе, каждое переименование заводило бы дубль со своей историей цен, своими
-- подписками и своим public_id. Ровно на этом мы уже горели (1207 дублей у YM,
-- миграции 030/031).
--
-- Поэтому ключ и ссылка для показа разъезжаются: url — канон /card/x/<oskuId>,
-- display_url — настоящий адрес со слагом. Пусто = показываем url.
-- ─────────────────────────────────────────────────────────────────────────────

ALTER TABLE products ADD COLUMN IF NOT EXISTS display_url TEXT;

COMMENT ON COLUMN products.display_url IS
    'Ссылка для показа пользователю, когда она отличается от канона url (Я.Маркет: настоящий слаг). NULL = показывать url.';

-- Перевод уже сохранённых /card/-ссылок Я.Маркета на новый канон. Их немного
-- (15 на момент миграции — их присылали пользователи), но без перевода они
-- разъедутся с новым каноном и при следующем добавлении заведут дубль.
--
-- Старый адрес со слагом уезжает в display_url, ключом становится /card/x/<id>.
-- Строки, у которых целевой канон уже занят другим товаром, НЕ трогаем: слияние
-- истории цен — отдельная операция, молча терять её нельзя.
UPDATE products p
SET display_url = p.url,
    url         = 'https://market.yandex.ru/card/x/' || substring(p.url from '/card/[^/]+/([0-9]+)'),
    updated_at  = NOW()
WHERE p.marketplace = 'yandex_market'
  AND p.url ~ '/card/[^/]+/[0-9]+'
  AND p.url !~ '^https://market\.yandex\.ru/card/x/[0-9]+$'
  AND NOT EXISTS (
      SELECT 1 FROM products q
      WHERE q.url = 'https://market.yandex.ru/card/x/' || substring(p.url from '/card/[^/]+/([0-9]+)')
  );

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
-- Обратный перевод: возвращаем показанную ссылку в ключ там, где она сохранена.
UPDATE products
SET url = display_url, updated_at = NOW()
WHERE marketplace = 'yandex_market' AND display_url IS NOT NULL;

ALTER TABLE products DROP COLUMN IF EXISTS display_url;
-- +goose StatementEnd
