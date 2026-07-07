#!/bin/sh
# Xvfb → Chromium работает headful (рабочий WebGL/canvas, без headless-сигналов,
# которые палит челлендж X5SEC), затем — сервис пула дорожек.
set -e
export DISPLAY="${DISPLAY:-:99}"

# Чистим stale-lock Xvfb (переживаем restart контейнера).
rm -f /tmp/.X11-unix/X"${DISPLAY#:}" /tmp/.X"${DISPLAY#:}"-lock 2>/dev/null || true

echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &

sleep 2
echo "[entrypoint] старт ali-miner"
exec python server.py
