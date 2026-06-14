#!/bin/sh
# Поднимаем виртуальный дисплей (Xvfb), чтобы Chromium работал headful, а не
# headless: на сервере без GPU это даёт рабочий WebGL/canvas/рендеринг шрифтов и
# убирает headless-сигналы, которые палит FAB-челлендж Ozon.
set -e
export DISPLAY="${DISPLAY:-:99}"
disp_num="${DISPLAY#:}"

# docker restart переиспользует контейнер → от прошлого Xvfb остаётся lock-файл,
# из-за которого новый Xvfb не стартует ("Missing X server"). Чистим перед стартом.
rm -f "/tmp/.X${disp_num}-lock" "/tmp/.X11-unix/X${disp_num}" 2>/dev/null || true

echo "[entrypoint] запускаю Xvfb на $DISPLAY"
Xvfb "$DISPLAY" -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/xvfb.log 2>&1 &

# дождаться, пока дисплей реально поднимется (иначе Chromium стартует раньше X)
for _ in $(seq 1 20); do
	[ -e "/tmp/.X11-unix/X${disp_num}" ] && break
	sleep 0.3
done

echo "[entrypoint] старт ozon-miner"
exec python miner.py
