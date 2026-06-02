# ── WB token miner ───────────────────────────────────────────────────────────
# python:3.12-slim + ОДИН Chromium (patchright) + Xvfb (виртуальный дисплей).
# Браузер крутится headful в Xvfb: на сервере без GPU это даёт рабочий WebGL и
# убирает headless-сигналы, по которым wbaas палит автоматизацию.
# Контейнер от root (внутренний сайдкар без проброшенных портов).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-browsers \
    DISPLAY=:99

WORKDIR /app

COPY requirements.txt .
# pip + Chromium + его OS-зависимости. (Этот слой кэшируется.)
RUN pip install -r requirements.txt \
 && patchright install --with-deps chromium \
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
COPY miner.py .
RUN chmod +x entrypoint.sh

ENTRYPOINT ["./entrypoint.sh"]
