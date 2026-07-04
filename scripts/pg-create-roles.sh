#!/usr/bin/env bash
# Создание не-суперюзер ролей Postgres (хардинг после инцидента 2026-07-03).
#
#   tryberry_app     — роль приложения: только DML (SELECT/INSERT/UPDATE/DELETE)
#                      на схему public базы tryberrybot. Без CREATE/SUPERUSER —
#                      компрометация сервиса больше не отдаёт весь кластер.
#   tryberry_monitor — роль postgres-exporter: pg_monitor, читает только статистику.
#
# Суперюзер `user` остаётся ТОЛЬКО для миграций (psql -U user) и pg_dump бэкапа.
#
# Запуск на сервере из каталога проекта (пароли берёт из .env):
#   ./scripts/pg-create-roles.sh
#
# Требует в .env: APP_DB_PASSWORD, MONITOR_DB_PASSWORD (сгенерировать:
#   openssl rand -base64 24 | tr -d '/+=' ). Идемпотентен: если роль уже есть —
# обновит пароль и перевыдаст гранты.
set -euo pipefail

cd "$(dirname "$0")/.."

ENV_FILE="${ENV_FILE:-.env}"
[ -f "$ENV_FILE" ] || { echo "нет $ENV_FILE — запускать из каталога проекта на сервере"; exit 1; }

# Достаём ровно две переменные, не экспортируя весь .env в шелл
# (см. граблю «set -a; . ./.env» в docs/SECURITY-HARDENING.md).
get_env() { grep -E "^$1=" "$ENV_FILE" | tail -1 | cut -d= -f2- | tr -d '"' ; }
APP_PASS="$(get_env APP_DB_PASSWORD)"
MON_PASS="$(get_env MONITOR_DB_PASSWORD)"
[ -n "$APP_PASS" ] || { echo "APP_DB_PASSWORD пуст/не задан в $ENV_FILE"; exit 1; }
[ -n "$MON_PASS" ] || { echo "MONITOR_DB_PASSWORD пуст/не задан в $ENV_FILE"; exit 1; }

PSQL="docker exec -i pt_postgres psql -U user -d tryberrybot -v ON_ERROR_STOP=1"

echo "== создаю/обновляю роли tryberry_app и tryberry_monitor =="
$PSQL <<SQL
-- Роль приложения: login + DML, ничего лишнего.
DO \$\$
BEGIN
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tryberry_app') THEN
    CREATE ROLE tryberry_app LOGIN;
  END IF;
  IF NOT EXISTS (SELECT FROM pg_roles WHERE rolname = 'tryberry_monitor') THEN
    CREATE ROLE tryberry_monitor LOGIN;
  END IF;
END
\$\$;

ALTER ROLE tryberry_app     LOGIN PASSWORD '$APP_PASS'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;
ALTER ROLE tryberry_monitor LOGIN PASSWORD '$MON_PASS'
  NOSUPERUSER NOCREATEDB NOCREATEROLE NOREPLICATION NOBYPASSRLS;

-- Приложение: доступ к базе и все DML на существующие объекты public.
GRANT CONNECT ON DATABASE tryberrybot TO tryberry_app;
GRANT USAGE ON SCHEMA public TO tryberry_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA public TO tryberry_app;
GRANT USAGE, SELECT, UPDATE ON ALL SEQUENCES IN SCHEMA public TO tryberry_app;

-- Будущие таблицы из миграций (их создаёт "user") видны приложению сразу —
-- без этого каждая миграция ломала бы прод до ручного GRANT.
ALTER DEFAULT PRIVILEGES FOR ROLE "user" IN SCHEMA public
  GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO tryberry_app;
ALTER DEFAULT PRIVILEGES FOR ROLE "user" IN SCHEMA public
  GRANT USAGE, SELECT, UPDATE ON SEQUENCES TO tryberry_app;

-- Мониторинг: только статистика.
GRANT pg_monitor TO tryberry_monitor;
GRANT CONNECT ON DATABASE tryberrybot TO tryberry_monitor;
SQL

echo "== проверка =="
docker exec pt_postgres psql -U user -d tryberrybot -c '\du'
echo "ok: роли готовы. Дальше — пересоздать сервисы с новым DATABASE_URL (см. docs/SECURITY-HARDENING.md)."
