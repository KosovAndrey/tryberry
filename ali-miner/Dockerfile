# ── ali-miner (browser-как-транспорт для aliexpress.ru) ──────────────────────
# По образцу ozon-miner, но ОТДЕЛЬНЫЙ сервис и БЕЗ прокси: Ali ходит напрямую с
# датацентр-IP VPS (Ozon — через прокси, поэтому их нельзя мешать в один образ).
# python:3.12-slim + ОДИН camoufox (анти-детект Firefox) + Xvfb. Браузер крутится
# headful в Xvfb: на сервере без GPU это даёт рабочий WebGL и убирает headless-
# сигналы, по которым антибот X5SEC палит автоматизацию.
#
# Сейчас образ несёт Phase-1 проб (probe.py — проверка, проходит ли camoufox
# X5SEC). Phase-2: добавится server.py (пул дорожек, GET /scrape) — как у
# ozon-miner — и CMD сменится на него.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-browsers \
    DISPLAY=:99

WORKDIR /app

COPY requirements.txt .
# pip + браузер + его OS-зависимости (слой кэшируется):
#  - camoufox fetch — скачивает анти-детект Firefox-сборку.
#  - playwright install-deps firefox — системные либы Firefox (GTK/X11/звук);
#    camoufox тянет playwright, поэтому CLI доступен. Это то, об что спотыкается
#    голый venv на хосте (libgtk-3.so.0 not found).
RUN pip install -r requirements.txt \
 && python -m camoufox fetch \
 && playwright install-deps firefox \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip

# ── Хотфикс бага playwright-firefox (juggler), как в ozon-miner ──────────────
# При uncaught page-error БЕЗ source-location обработчик FFPage._onUncaughtError
# безусловно читает pageError.location.url у undefined → НЕОБРАБОТАННЫЙ TypeError
# роняет весь Node-драйвер, и соединение playwright умирает. Делаем доступ к
# .location опциональным прямо в бандле. Патч применяем, ТОЛЬКО если паттерн на
# месте (если версия playwright сменилась — не валим билд, просто пропускаем;
# у нового сервиса это безопаснее, чем жёсткий fail ozon-miner).
RUN set -eu; \
    bundle="$(python -c 'import os,playwright; print(os.path.join(os.path.dirname(playwright.__file__),"driver","package","lib","coreBundle.js"))')"; \
    if grep -q 'pageError\.location\.url' "$bundle"; then \
      sed -i 's/pageError\.location\.\([A-Za-z]\+\)/pageError.location?.\1/g' "$bundle"; \
      echo "coreBundle.js: pageError.location guard applied → $bundle"; \
    else \
      echo "coreBundle.js: паттерн не найден (версия playwright сменилась?) — патч пропущен"; \
    fi

# Отдельным слоем: Xvfb, программный GL (Mesa, на VPS без GPU) и кириллица.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb libgl1-mesa-dri fonts-liberation fonts-dejavu-core \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh probe.py ./
RUN chmod +x entrypoint.sh

ENTRYPOINT ["./entrypoint.sh"]
# Phase-1: проб. Phase-2 сменить на ["python", "server.py"].
CMD ["python", "probe.py"]
