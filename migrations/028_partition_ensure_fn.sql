-- ─────────────────────────────────────────────────────────────────────────────
-- 028_partition_ensure_fn.sql — SECURITY DEFINER для месячных партиций.
--
-- После хардинга 2026-07-03 роль приложения tryberry_app — строго DML
-- (scripts/pg-create-roles.sh), прямой CREATE TABLE ей запрещён (42501).
-- Но partition.Manager внутри scraper должен уметь создавать партиции
-- price_history (месяц вперёд по тику + прошлые месяцы под бэкфилл WB).
--
-- Решение: функция от владельца-суперюзера, исполняется его правами
-- (SECURITY DEFINER) и умеет РОВНО одно — создать месячную партицию
-- price_history. Это не расширяет DDL-поверхность app-роли: GRANT CREATE
-- ON SCHEMA + владение price_history дали бы ей в т.ч. DROP основной таблицы.
--
-- Гонка двух реплик (duplicate_table при параллельном CREATE) гасится внутри —
-- вызывающему функция всегда идемпотентна.
--
-- На проде накатывается руками psql от суперюзера "user" (goose на проде не
-- используется) — см. README-deploy.md.
-- ─────────────────────────────────────────────────────────────────────────────

-- +goose Up
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION ensure_price_history_partition(p_month date)
RETURNS void
LANGUAGE plpgsql
SECURITY DEFINER
-- фиксируем search_path: обязательный приём для SECURITY DEFINER, чтобы
-- вызывающий не подсунул свои объекты через изменённый путь поиска
SET search_path = public, pg_temp
AS $fn$
DECLARE
    v_from date := date_trunc('month', p_month)::date;
    v_to   date := (date_trunc('month', p_month) + interval '1 month')::date;
    v_name text := format('price_history_%s', to_char(v_from, 'YYYY_MM'));
BEGIN
    EXECUTE format(
        'CREATE TABLE IF NOT EXISTS %I PARTITION OF price_history FOR VALUES FROM (%L) TO (%L)',
        v_name, v_from, v_to);
EXCEPTION
    WHEN duplicate_table THEN
        NULL; -- параллельный процесс успел первым: партиция есть — это успех
END;
$fn$;
-- +goose StatementEnd

-- Дефолтный ACL функций отдаёт EXECUTE всем — сужаем до app-роли.
REVOKE ALL ON FUNCTION ensure_price_history_partition(date) FROM PUBLIC;

-- +goose StatementBegin
-- На локальных/тестовых базах роли tryberry_app может не быть — не падаем.
DO $do$
BEGIN
    IF EXISTS (SELECT FROM pg_roles WHERE rolname = 'tryberry_app') THEN
        GRANT EXECUTE ON FUNCTION ensure_price_history_partition(date) TO tryberry_app;
    END IF;
END
$do$;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS ensure_price_history_partition(date);
