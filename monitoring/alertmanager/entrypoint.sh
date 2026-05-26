#!/bin/sh
set -e

sed -e "s|\${TELEGRAM_ALERT_BOT_TOKEN}|$TELEGRAM_ALERT_BOT_TOKEN|g" \
    -e "s|\${TELEGRAM_ALERT_CHAT_ID}|$TELEGRAM_ALERT_CHAT_ID|g" \
    /config/alertmanager.yml > /tmp/alertmanager.yml

exec /bin/alertmanager \
  --config.file=/tmp/alertmanager.yml \
  --storage.path=/alertmanager \
  --web.external-url=http://localhost:9093