#!/bin/sh
# Поднимаем виртуальный дисплей (Xvfb), чтобы Firefox/camoufox работал headful, а
# не headless: на сервере без GPU это даёт рабочий WebGL/canvas и убирает
# headless-сигналы, по которым палится антибот X5SEC. Затем — переданная команда
# (CMD: проб или, в Phase-2, server.py).
set -e
export DISPLAY="${DISPLAY:-:99}"

# Чистим stale-lock Xvfb (переживаем restart контейнера, как в ozon-miner).
rm -f /tmp/.X11-unix/X"${DISPLAY#:}" /tmp/.X"${DISPLAY#:}"-lock 2>/dev/null || true

echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &

# дать дисплею подняться
sleep 2
echo "[entrypoint] exec: $*"
exec "$@"
