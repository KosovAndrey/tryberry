-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- 003_search.sql — поиск по запросу с фильтрами
-- ─────────────────────────────────────────────────────────────────────────────
--
-- Добавляет 5 таблиц для нового типа подписки: «отслеживать выдачу поиска».
-- Пользователь кидает ссылку поиска WB → бот скрейпит её каждый цикл → шлёт
-- уведомления когда товары снижаются по выбранному правилу.
--
-- Дедупликация:
--  • search_queries.normalized_url UNIQUE — один запрос на всех подписчиков
--  • products.url UNIQUE (есть из 001) — один товар везде в БД
--  • search_results PK (search_query_id, product_id) — товар не повторяется
--
-- Триггеры уведомлений (3 типа, выбирает пользователь при создании подписки):
--  • below_target  — target_price, обязательно поле target_price
--  • any_drop      — любое снижение
--  • discount_pct  — N% от first_seen_price, обязательно поле discount_pct
-- После первого срабатывания все триггеры ведут себя одинаково: «дальше
-- уведомлять при любом снижении от цены последнего уведомления».
-- ─────────────────────────────────────────────────────────────────────────────


-- 1. Поисковые запросы — шарятся между пользователями
CREATE TABLE IF NOT EXISTS search_queries (
    id              BIGSERIAL PRIMARY KEY,
    marketplace     TEXT NOT NULL,
    normalized_url  TEXT NOT NULL UNIQUE,         -- ключ дедупликации
    query_text      TEXT NOT NULL,                -- "iphone 16" — для UI
    filters         JSONB NOT NULL DEFAULT '{}'::jsonb,
    last_scraped_at TIMESTAMPTZ,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_search_queries_marketplace
    ON search_queries (marketplace);

-- Для планировщика «кого скрейпить дальше» — нулы (никогда не скрейпили) первыми
CREATE INDEX IF NOT EXISTS idx_search_queries_last_scraped
    ON search_queries (last_scraped_at NULLS FIRST);


-- 2. Подписки пользователей на поисковые запросы
CREATE TABLE IF NOT EXISTS search_subscriptions (
    id               BIGSERIAL PRIMARY KEY,
    user_id          BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    search_query_id  BIGINT NOT NULL REFERENCES search_queries(id) ON DELETE CASCADE,
    trigger_type     TEXT NOT NULL
                     CHECK (trigger_type IN ('below_target', 'any_drop', 'discount_pct')),
    target_price     NUMERIC(12, 2),       -- для below_target
    discount_pct     SMALLINT,             -- для discount_pct (1..99)
    active           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    updated_at       TIMESTAMPTZ NOT NULL DEFAULT NOW(),

    -- Гарантируем что для каждого типа триггера есть необходимое поле
    CONSTRAINT chk_below_target_has_price
        CHECK (trigger_type <> 'below_target' OR target_price IS NOT NULL),
    CONSTRAINT chk_discount_pct_has_pct
        CHECK (trigger_type <> 'discount_pct'
               OR (discount_pct IS NOT NULL AND discount_pct BETWEEN 1 AND 99))
);

-- Один и тот же запрос можно подписать с разными триггерами — поэтому
-- уникальности (user_id, search_query_id) НЕТ, в отличие от обычных subscriptions.
-- Если позже захочется ограничить — добавим уникальный индекс по trigger_type тоже.

CREATE INDEX IF NOT EXISTS idx_search_subs_user
    ON search_subscriptions (user_id) WHERE active;

CREATE INDEX IF NOT EXISTS idx_search_subs_query
    ON search_subscriptions (search_query_id) WHERE active;


-- 3. Текущая выдача каждого запроса (обновляется каждым скрейпом)
-- При новом скрейпе делаем UPSERT: товары которые исчезли — last_seen_at не
-- обновится, потом их можно «забыть» (TTL) — это уже application logic.
CREATE TABLE IF NOT EXISTS search_results (
    search_query_id  BIGINT NOT NULL REFERENCES search_queries(id) ON DELETE CASCADE,
    product_id       BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    position         INTEGER NOT NULL,       -- позиция в выдаче (для сортировки)
    last_price       NUMERIC(12, 2) NOT NULL,
    first_seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (search_query_id, product_id)
);

-- Для запроса «дай свежую выдачу запроса X в порядке позиции»
CREATE INDEX IF NOT EXISTS idx_search_results_query_pos
    ON search_results (search_query_id, position);


-- 4. Снимок «первой виденной цены» для каждой подписки и каждого товара.
-- Используется для первого срабатывания триггера (особенно discount_pct).
-- После первого уведомления роль baseline переходит к search_notifications.
--
-- Заполняется в двух случаях:
--  • при создании подписки — для всех товаров текущей выдачи
--  • при появлении в выдаче нового товара (которого раньше не было) —
--    для каждой активной подписки на этот запрос
CREATE TABLE IF NOT EXISTS search_subscription_products (
    subscription_id   BIGINT NOT NULL REFERENCES search_subscriptions(id) ON DELETE CASCADE,
    product_id        BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    first_seen_price  NUMERIC(12, 2) NOT NULL,
    first_seen_at     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (subscription_id, product_id)
);


-- 5. История уведомлений по поиск-подпискам.
-- Двойная роль:
--   а) лог отправленных сообщений (защита от дублей)
--   б) источник baseline для последующих сравнений (цена последнего
--      уведомления = от чего отсчитываем дальше)
CREATE TABLE IF NOT EXISTS search_notifications (
    id              BIGSERIAL PRIMARY KEY,
    subscription_id BIGINT NOT NULL REFERENCES search_subscriptions(id) ON DELETE CASCADE,
    product_id      BIGINT NOT NULL REFERENCES products(id) ON DELETE CASCADE,
    price           NUMERIC(12, 2) NOT NULL,
    sent_at         TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Главный «горячий» запрос: «дай ЦЕНУ последнего уведомления для пары
-- (подписка, товар)» — нужен на каждом цикле триггеров для каждого товара.
-- DESC чтобы первая строка индекса уже была самой свежей.
CREATE INDEX IF NOT EXISTS idx_search_notifications_lookup
    ON search_notifications (subscription_id, product_id, sent_at DESC);

-- +goose StatementEnd


-- +goose Down
-- +goose StatementBegin

DROP TABLE IF EXISTS search_notifications;
DROP TABLE IF EXISTS search_subscription_products;
DROP TABLE IF EXISTS search_results;
DROP TABLE IF EXISTS search_subscriptions;
DROP TABLE IF EXISTS search_queries;

-- +goose StatementEnd
