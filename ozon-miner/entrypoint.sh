#!/bin/sh
# Поднимаем виртуальный дисплей (Xvfb), чтобы Chromium работал headful, а не
# headless: на сервере без GPU это даёт рабочий WebGL/canvas/рендеринг шрифтов и
# убирает headless-сигналы, которые палит FAB (антибот Ozon). Затем — сервис пула.
set -e
export DISPLAY="${DISPLAY:-:99}"

# Чистим stale-lock Xvfb (переживаем restart контейнера, как в WB-майнере).
rm -f /tmp/.X11-unix/X"${DISPLAY#:}" /tmp/.X"${DISPLAY#:}"-lock 2>/dev/null || true

echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &

# дать дисплею подняться
sleep 2
echo "[entrypoint] старт сервиса пула дорожек"
exec python server.py
