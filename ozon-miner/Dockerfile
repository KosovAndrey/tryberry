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

COPY requirements.txt .
# pip + браузеры + их OS-зависимости. (Этот слой кэшируется.)
#  - patchright install chromium — запасной Chromium-движок (+ его deps).
#  - camoufox fetch — скачивает анти-детект Firefox-сборку (основной движок).
#  - playwright install-deps firefox — системные либы для Firefox/camoufox
#    (camoufox тянет playwright, поэтому CLI доступен; ставит GTK/X11/звук и пр.).
RUN pip install -r requirements.txt \
 && patchright install --with-deps chromium \
 && python -m camoufox fetch \
 && playwright install-deps firefox \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip

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
