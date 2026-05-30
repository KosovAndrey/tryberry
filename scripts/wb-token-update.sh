#!/usr/bin/env bash
# wb-token-update.sh — обновить WB-токен поиска в Redis из вставленного cURL.
#
# Использование:
#   ./scripts/wb-token-update.sh           # вставить cURL, Enter, затем Ctrl-D
#   ./scripts/wb-token-update.sh file.txt  # прочитать cURL из файла
#
# Из браузера: DevTools → Network → запрос u-search → ПКМ → Copy → Copy as cURL (bash).
# Скрипт извлекает строку cookie и user-agent, проверяет наличие x_wbaas_token и
# пишет в Redis (контейнер pt_redis): wb:search:cookie, wb:search:ua, wb:search:token
set -euo pipefail

REDIS_CONTAINER="${REDIS_CONTAINER:-pt_redis}"
KEY_COOKIE="wb:search:cookie"
KEY_UA="wb:search:ua"
KEY_TOKEN="wb:search:token"

if [ -z "${1:-}" ]; then
  echo "Вставьте cURL (Copy as cURL (bash)) из браузера, затем Enter и Ctrl-D:" >&2
fi
blob="$(cat "${1:-/dev/stdin}")"

# cookie: -b 'value' | -b "value" | -H 'cookie: value'
cookie="$(printf '%s' "$blob" | grep -oP "(?<=-b ')[^']*" | head -n1 || true)"
[ -z "$cookie" ] && cookie="$(printf '%s' "$blob" | grep -oP '(?<=-b ")[^"]*' | head -n1 || true)"
[ -z "$cookie" ] && cookie="$(printf '%s' "$blob" | grep -oiP "(?<=-H 'cookie: )[^']*" | head -n1 || true)"
[ -z "$cookie" ] && cookie="$(printf '%s' "$blob" | grep -oiP '(?<=-H "cookie: )[^"]*' | head -n1 || true)"

# user-agent: -H 'user-agent: value' (любой регистр)
ua="$(printf '%s' "$blob" | grep -oiP "(?<=-H 'user-agent: )[^']*" | head -n1 || true)"
[ -z "$ua" ] && ua="$(printf '%s' "$blob" | grep -oiP '(?<=-H "user-agent: )[^"]*' | head -n1 || true)"

# token из cookie (или из всего blob как запасной вариант)
token="$(printf '%s' "$cookie" | grep -oP 'x_wbaas_token=\K[^;]+' | head -n1 || true)"
[ -z "$token" ] && token="$(printf '%s' "$blob" | grep -oP "x_wbaas_token=\K[^;'\" ]+" | head -n1 || true)"

if [ -z "$token" ]; then
  echo "ОШИБКА: x_wbaas_token не найден. Redis не изменён." >&2
  echo "Проверь, что копировал через 'Copy as cURL (bash)' и в запросе есть кука x_wbaas_token." >&2
  exit 1
fi
[ -z "$cookie" ] && cookie="x_wbaas_token=$token"

# (необязательно) срок жизни токена из payload
exp="$(printf '%s' "$token" | awk -F. '{print $4}' | base64 -d 2>/dev/null \
        | tr '|' '\n' | grep -oE '^17[0-9]{8}$' | sort -n | tail -n1 || true)"
if [ -n "${exp:-}" ]; then
  echo "Токен (ориентировочно) действует примерно до: $(date -d "@$exp" 2>/dev/null || echo ts=$exp)" >&2
fi

if ! docker ps --format '{{.Names}}' | grep -qx "$REDIS_CONTAINER"; then
  echo "ОШИБКА: контейнер Redis '$REDIS_CONTAINER' не запущен. Извлечённый токен:" >&2
  echo "  $token" >&2
  exit 1
fi

docker exec -i "$REDIS_CONTAINER" redis-cli SET "$KEY_COOKIE" "$cookie" >/dev/null
docker exec -i "$REDIS_CONTAINER" redis-cli SET "$KEY_UA"     "$ua"     >/dev/null
docker exec -i "$REDIS_CONTAINER" redis-cli SET "$KEY_TOKEN"  "$token"  >/dev/null

echo "OK: токен обновлён в Redis ($REDIS_CONTAINER)." >&2
echo "  $KEY_TOKEN = ${token:0:24}…(${#token} симв.)" >&2
echo "  $KEY_UA    = ${ua:-<пусто → будет дефолтный UA>}" >&2
