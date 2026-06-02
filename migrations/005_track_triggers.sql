-- +goose Up
-- +goose StatementBegin

-- Стратегии триггера для ОДИНОЧНЫХ товарных подписок (как у поиск-подписок).
-- baseline_price остаётся «последней опорной ценой»: при подписке = first_seen_price,
-- после каждого уведомления обновляется на цену уведомления (поведение any_drop
-- для существующих строк сохраняется без изменений).
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS trigger_type     TEXT NOT NULL DEFAULT 'any_drop';
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS target_price     NUMERIC(12,2);                 -- для below_target
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS discount_pct     SMALLINT;                      -- для discount_pct (1..99)
-- first_seen_price — НЕИЗМЕННАЯ цена на момент подписки. База первого
-- срабатывания для discount_pct и any_drop (порог скидки не должен «уплывать»).
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS first_seen_price NUMERIC(12,2) NOT NULL DEFAULT 0;
-- notified — было ли уже отправлено хотя бы одно уведомление по подписке.
-- Определяет фазу: первое срабатывание (по типу триггера) vs повторное (любое
-- снижение от цены последнего уведомления).
ALTER TABLE subscriptions ADD COLUMN IF NOT EXISTS notified         BOOLEAN NOT NULL DEFAULT FALSE;

-- Бэкфилл для уже существующих подписок: стартовая цена = текущий baseline.
UPDATE subscriptions SET first_seen_price = baseline_price WHERE first_seen_price = 0;

-- Констрейнты-зеркала search_subscriptions (идемпотентно: drop+add).
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_trigger_type;
ALTER TABLE subscriptions ADD  CONSTRAINT chk_sub_trigger_type
    CHECK (trigger_type IN ('below_target','any_drop','discount_pct'));

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_below_target_has_price;
ALTER TABLE subscriptions ADD  CONSTRAINT chk_sub_below_target_has_price
    CHECK (trigger_type <> 'below_target' OR target_price IS NOT NULL);

ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_discount_pct_has_pct;
ALTER TABLE subscriptions ADD  CONSTRAINT chk_sub_discount_pct_has_pct
    CHECK (trigger_type <> 'discount_pct'
           OR (discount_pct IS NOT NULL AND discount_pct BETWEEN 1 AND 99));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_trigger_type;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_below_target_has_price;
ALTER TABLE subscriptions DROP CONSTRAINT IF EXISTS chk_sub_discount_pct_has_pct;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS notified;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS first_seen_price;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS discount_pct;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS target_price;
ALTER TABLE subscriptions DROP COLUMN IF EXISTS trigger_type;
-- +goose StatementEnd
