#!/usr/bin/env bash
# ────────────────────────────────────────────────────────────────────────────
# init-letsencrypt.sh
#
# Первичный выпуск сертификата Let's Encrypt для tryberry.ru и поддоменов.
# После первого запуска certbot будет обновлять сертификаты автоматически.
#
# Использование:
#   bash scripts/init-letsencrypt.sh           # боевой Let's Encrypt
#   STAGING=1 bash scripts/init-letsencrypt.sh # staging (для отладки)
# ────────────────────────────────────────────────────────────────────────────

set -euo pipefail

# ── Конфигурация ────────────────────────────────────────────────────────────

DOMAINS=(tryberry.ru www.tryberry.ru grafana.tryberry.ru)
EMAIL="${CERTBOT_EMAIL:-}"
RSA_KEY_SIZE=4096
DATA_PATH="./nginx/certbot"
STAGING="${STAGING:-0}"

COMPOSE="docker compose -f docker-compose.yml -f docker-compose.prod.yml"

# ── Цвета ───────────────────────────────────────────────────────────────────

green()  { printf "\033[0;32m%s\033[0m\n" "$*"; }
yellow() { printf "\033[0;33m%s\033[0m\n" "$*"; }
red()    { printf "\033[0;31m%s\033[0m\n" "$*"; }

# ── Sanity checks ───────────────────────────────────────────────────────────

if [ -z "$EMAIL" ]; then
    red "❌ CERTBOT_EMAIL не задан. Положи в .env: CERTBOT_EMAIL=your@email.com"
    red "   Затем перед запуском скрипта: set -a; source .env; set +a"
    exit 1
fi

if ! command -v docker >/dev/null 2>&1; then
    red "❌ docker не установлен"
    exit 1
fi

if ! docker compose version >/dev/null 2>&1; then
    red "❌ docker compose plugin не установлен"
    exit 1
fi

if [ ! -f "./nginx/conf.d/tryberry.conf" ]; then
    red "❌ Запускай из корня проекта (./nginx/conf.d/tryberry.conf не найден)"
    exit 1
fi

if [ ! -f "./nginx/.htpasswd" ]; then
    red "❌ ./nginx/.htpasswd не найден. Создай:  htpasswd -c nginx/.htpasswd admin"
    exit 1
fi

# ── Проверка что сертификаты уже не выпущены ───────────────────────────────

if [ -d "$DATA_PATH/conf/live/${DOMAINS[0]}" ]; then
    yellow "⚠️  Найдены существующие сертификаты для ${DOMAINS[0]}"
    read -p "    Перезаписать? (y/N) " decision
    if [ "$decision" != "Y" ] && [ "$decision" != "y" ]; then
        green "Окей, ничего не делаем."
        exit 0
    fi
fi

# ── Скачиваем рекомендованные TLS-настройки от Certbot ─────────────────────

if [ ! -e "$DATA_PATH/conf/options-ssl-nginx.conf" ] || \
   [ ! -e "$DATA_PATH/conf/ssl-dhparams.pem" ]; then
    green "### Скачиваем рекомендованные TLS-параметры..."
    mkdir -p "$DATA_PATH/conf"
    curl -fsSL \
        https://raw.githubusercontent.com/certbot/certbot/master/certbot-nginx/certbot_nginx/_internal/tls_configs/options-ssl-nginx.conf \
        > "$DATA_PATH/conf/options-ssl-nginx.conf"
    curl -fsSL \
        https://raw.githubusercontent.com/certbot/certbot/master/certbot/certbot/ssl-dhparams.pem \
        > "$DATA_PATH/conf/ssl-dhparams.pem"
fi

# ── Шаг 1. Создаём dummy-сертификат ─────────────────────────────────────────
# nginx не стартует без существующего ssl_certificate, а certbot не выпустит
# настоящий cert без работающего nginx. Поэтому самоподписанная заглушка.

PRIMARY="${DOMAINS[0]}"
CERT_DIR="$DATA_PATH/conf/live/$PRIMARY"

green "### Создаём dummy-сертификат для $PRIMARY..."
mkdir -p "$CERT_DIR"
$COMPOSE run --rm --entrypoint "\
    openssl req -x509 -nodes -newkey rsa:2048 -days 1 \
        -keyout '/etc/letsencrypt/live/$PRIMARY/privkey.pem' \
        -out    '/etc/letsencrypt/live/$PRIMARY/fullchain.pem' \
        -subj   '/CN=localhost'" certbot

# ── Шаг 2. Стартуем nginx ───────────────────────────────────────────────────

green "### Стартуем nginx..."
$COMPOSE up --force-recreate -d nginx
sleep 3

# ── Шаг 3. Удаляем dummy ────────────────────────────────────────────────────

green "### Удаляем dummy-сертификат..."
$COMPOSE run --rm --entrypoint "\
    sh -c 'rm -rf /etc/letsencrypt/live/$PRIMARY \
                  /etc/letsencrypt/archive/$PRIMARY \
                  /etc/letsencrypt/renewal/$PRIMARY.conf'" certbot

# ── Шаг 4. Запрашиваем настоящий сертификат ─────────────────────────────────

DOMAIN_ARGS=""
for d in "${DOMAINS[@]}"; do
    DOMAIN_ARGS="$DOMAIN_ARGS -d $d"
done

STAGING_FLAG=""
if [ "$STAGING" != "0" ]; then
    STAGING_FLAG="--staging"
    yellow "⚠️  STAGING режим — сертификат будет невалидным для браузеров"
fi

green "### Запрашиваем сертификат у Let's Encrypt..."
$COMPOSE run --rm --entrypoint "\
    certbot certonly --webroot -w /var/www/certbot \
        $STAGING_FLAG \
        $DOMAIN_ARGS \
        --email $EMAIL \
        --rsa-key-size $RSA_KEY_SIZE \
        --agree-tos \
        --no-eff-email \
        --force-renewal" certbot

# ── Шаг 5. Reload nginx ────────────────────────────────────────────────────

green "### Reload nginx..."
$COMPOSE exec nginx nginx -s reload

green ""
green "✅ Готово! Проверь:"
green "    curl -I https://tryberry.ru"
green "    curl -I https://grafana.tryberry.ru"
