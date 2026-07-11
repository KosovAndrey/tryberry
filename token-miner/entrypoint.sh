#!/bin/sh
# Поднимаем виртуальный дисплей (Xvfb), чтобы Chromium работал headful, а не
# headless: на сервере без GPU это даёт рабочий WebGL/canvas/рендеринг шрифтов и
# убирает headless-сигналы, которые палит challenge_fingerprint.js у wbaas.
set -e
export DISPLAY="${DISPLAY:-:99}"

# Чистим stale-lock Xvfb (переживаем restart контейнера): без этого после
# docker restart старый /tmp/.X99-lock не даёт Xvfb подняться, и майнер
# бесконечно фейлит "Missing X server or $DISPLAY" (инцидент 2026-07-11).
rm -f /tmp/.X11-unix/X"${DISPLAY#:}" /tmp/.X"${DISPLAY#:}"-lock 2>/dev/null || true

echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &
# дать дисплею подняться
sleep 2
echo "[entrypoint] старт майнера"
exec python miner.py
