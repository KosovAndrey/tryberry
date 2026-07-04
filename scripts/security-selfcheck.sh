#!/usr/bin/env bash
# Периодическая самопроверка на признаки компрометации (после инцидента
# 2026-07-03). Запускается кроном на хосте (см. install-указания внизу).
# При любом отклонении шлёт алерт в Telegram (бот/чат — те же, что у
# Alertmanager: TELEGRAM_ALERT_BOT_TOKEN / TELEGRAM_ALERT_CHAT_ID из .env).
#
# Дублирует Prometheus-алерты (rules/security.yml) снаружи стека: если атакующий
# положил/подменил мониторинг — этот скрипт всё равно отработает, и наоборот.
#
# Проверки:
#   1. Published-порты docker: наружу (0.0.0.0/::) только nginx 80/443.
#   2. Redis: role=master, REPLICAOF отключён (rename-command).
#   3. Postgres: ровно 1 суперюзер ('user'), login-роли — только ожидаемые.
#   4. UFW активен; в DOCKER-USER есть DROP.
#
# Установка (раз в 15 минут + лог):
#   sudo tee /etc/cron.d/tryberry-selfcheck >/dev/null <<'CRON'
#   */15 * * * * root /home/kosovandrey/projects/tryberrybot/scripts/security-selfcheck.sh >> /var/log/tryberry-selfcheck.log 2>&1
#   CRON
set -uo pipefail

# Под cron PATH урезан до /usr/bin:/bin — ufw/iptables из /usr/sbin не находятся
# и проверка фаервола ложно алертит «UFW не активен».
export PATH="/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"

PROJECT_DIR="$(cd "$(dirname "$0")/.." && pwd)"
ENV_FILE="$PROJECT_DIR/.env"

get_env() { grep -E "^$1=" "$ENV_FILE" 2>/dev/null | tail -1 | cut -d= -f2- | tr -d '"' ; }

FAILURES=""
fail() { FAILURES="${FAILURES}• $1"$'\n'; }

# ── 1. порты наружу ──────────────────────────────────────────────────────────
# Любой published-порт на 0.0.0.0/[::], кроме nginx 80/443 — тревога.
OPEN_PORTS="$(docker ps --format '{{.Names}}: {{.Ports}}' 2>/dev/null | tr ',' '\n' \
  | grep -E '0\.0\.0\.0|\[::\]' \
  | grep -vE '(^|\s)(tryberry_nginx:\s*)?(0\.0\.0\.0|\[::\]):(80|443)->' || true)"
[ -n "$OPEN_PORTS" ] && fail "открытые наружу docker-порты (ждём только nginx 80/443): $OPEN_PORTS"

# ── 2. redis ─────────────────────────────────────────────────────────────────
if docker ps --format '{{.Names}}' | grep -q '^pt_redis$'; then
  ROLE="$(docker exec pt_redis redis-cli role 2>/dev/null | head -1)"
  [ "$ROLE" = "master" ] || fail "redis role='$ROLE' (не master — rogue-replica?)"
  R="$(docker exec pt_redis redis-cli REPLICAOF NO ONE 2>&1 || true)"
  echo "$R" | grep -q 'unknown command' || fail "redis: REPLICAOF не отключён (ответ: $R)"
else
  fail "контейнер pt_redis не запущен"
fi

# ── 3. postgres роли ─────────────────────────────────────────────────────────
if docker ps --format '{{.Names}}' | grep -q '^pt_postgres$'; then
  SUPERS="$(docker exec pt_postgres psql -U user -d tryberrybot -tAc \
    "SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolsuper AND rolname NOT LIKE 'pg\_%'" 2>/dev/null)"
  [ "$SUPERS" = "user" ] || fail "pg суперюзеры: '$SUPERS' (ждём только 'user' — бэкдор?)"
  LOGINS="$(docker exec pt_postgres psql -U user -d tryberrybot -tAc \
    "SELECT string_agg(rolname, ',' ORDER BY rolname) FROM pg_roles WHERE rolcanlogin AND rolname NOT LIKE 'pg\_%'" 2>/dev/null)"
  [ "$LOGINS" = "tryberry_app,tryberry_monitor,user" ] || fail "pg login-роли: '$LOGINS' (ждём tryberry_app,tryberry_monitor,user)"
else
  fail "контейнер pt_postgres не запущен"
fi

# ── 4. фаервол ───────────────────────────────────────────────────────────────
ufw status 2>/dev/null | grep -q 'Status: active' || fail "UFW не активен"
iptables -L DOCKER-USER -n 2>/dev/null | grep -q 'DROP' || fail "в DOCKER-USER нет DROP-правила (docker-порты не прикрыты)"

# ── итог ─────────────────────────────────────────────────────────────────────
TS="$(date '+%F %T')"
if [ -z "$FAILURES" ]; then
  echo "$TS ok"
  exit 0
fi

echo "$TS FAIL:"
echo "$FAILURES"

BOT="$(get_env TELEGRAM_ALERT_BOT_TOKEN)"
CHAT="$(get_env TELEGRAM_ALERT_CHAT_ID)"
if [ -n "$BOT" ] && [ -n "$CHAT" ]; then
  TEXT="🚨 <b>security-selfcheck: отклонения на $(hostname)</b>

$FAILURES
docs/SECURITY-HARDENING.md → «Быстрая проверка»"
  curl -sS --max-time 15 "https://api.telegram.org/bot${BOT}/sendMessage" \
    --data-urlencode "chat_id=${CHAT}" \
    --data-urlencode "text=${TEXT}" \
    --data-urlencode "parse_mode=HTML" >/dev/null \
    || echo "$TS не смог отправить алерт в Telegram"
fi
exit 1
