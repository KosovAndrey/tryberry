#!/bin/sh
# Поднимаем виртуальный дисплей (Xvfb), чтобы Chromium работал headful, а не
# headless: на сервере без GPU это даёт рабочий WebGL/canvas/рендеринг шрифтов и
# убирает headless-сигналы, которые палит FAB-челлендж Ozon.
set -e
export DISPLAY="${DISPLAY:-:99}"
echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &
# дать дисплею подняться
sleep 2
echo "[entrypoint] старт ozon-miner"
exec python miner.py
