# ── Ozon miner (browser-как-транспорт, пул дорожек) ──────────────────────────
# python:3.12-slim + ОДИН Chromium (patchright) + Xvfb (виртуальный дисплей).
# Браузер крутится headful в Xvfb: на сервере без GPU это даёт рабочий WebGL и
# убирает headless-сигналы, по которым FAB (антибот Ozon) палит автоматизацию.
# Тот же базовый образ и стек, что у token-miner (WB) — проверен в проде.
# Один контейнер держит ПУЛ дорожек (по браузеру на дорожку), портов наружу нет —
# к нему ходит только Go-скрейпер по внутренней docker-сети (GET /scrape).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-browsers \
    DISPLAY=:99

WORKDIR /app

COPY requirements.txt fetch_addons.py ./
# pip + браузеры + их OS-зависимости. (Этот слой кэшируется.)
#  - patchright install chromium — запасной Chromium-движок (+ его deps).
#  - camoufox fetch — скачивает анти-детект Firefox-сборку (основной движок).
#  - fetch_addons.py — ДОкачивает дефолтные аддоны camoufox с проверкой и роняет
#    сборку, если не вышло: сам `camoufox fetch` неудачу закачки только печатает
#    и оставляет пустой addons/UBO, из-за чего браузер потом не стартует вовсе
#    (инцидент 07-09-2026 — пул умер целиком на "manifest.json is missing").
#  - playwright install-deps firefox — системные либы для Firefox/camoufox
#    (camoufox тянет playwright, поэтому CLI доступен; ставит GTK/X11/звук и пр.).
RUN pip install -r requirements.txt \
 && patchright install --with-deps chromium \
 && python -m camoufox fetch \
 && python fetch_addons.py \
 && playwright install-deps firefox \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip

# ── Хотфикс бага playwright-firefox (juggler), 21.06.2026 ────────────────────
# При uncaught page-error БЕЗ source-location обработчик FFPage._onUncaughtError →
# _Page.addPageError безусловно читает pageError.location.url у undefined →
# НЕОБРАБОТАННЫЙ TypeError роняет весь Node-процесс драйвера (coreBundle.js).
# После этого playwright-соединение мертво и ВЕСЬ пул дорожек уходит в 100%
# ошибок ('Connection closed while reading from the driver'). Ozon начал отдавать
# такие ошибки 21.06.2026; апгрейд playwright НЕ помогает (баг есть и в актуальной
# сборке), а из Python краш не перехватить — он в самом драйвере до доставки
# события. Делаем доступ к .location опциональным прямо в бандле.
# set -e + grep-проверки: билд ПАДАЕТ, если паттерн исчез (значит версия
# playwright сменилась — патч надо пересмотреть, а не молча потерять).
RUN set -eu; \
    bundle="$(python -c 'import os,playwright; print(os.path.join(os.path.dirname(playwright.__file__),"driver","package","lib","coreBundle.js"))')"; \
    grep -q 'pageError\.location\.url' "$bundle"; \
    sed -i 's/pageError\.location\.\([A-Za-z]\+\)/pageError.location?.\1/g' "$bundle"; \
    ! grep -q 'pageError\.location\.url' "$bundle"; \
    echo "coreBundle.js: pageError.location guard applied → $bundle"

# Отдельным слоем: Xvfb, программный GL (Mesa) и шрифты с кириллицей.
# Идёт ПОСЛЕ слоя выше, чтобы не сбивать кэш скачивания Chromium.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb libgl1-mesa-dri fonts-liberation fonts-dejavu-core \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh .
COPY server.py .
COPY probe.py .
RUN chmod +x entrypoint.sh

ENTRYPOINT ["./entrypoint.sh"]
