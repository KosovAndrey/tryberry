# ── WB search miner (браузер-как-транспорт) ──────────────────────────────────
# python:3.12-slim + Chromium (patchright) + Xvfb. Тот же движок и приём, что у
# token-miner (headful в Xvfb проходит wbaas), но сервис долгоживущий: держит
# прогретые дорожки и отдаёт выдачу u-search in-page fetch'ем по HTTP (aiohttp).
# Внутренний сайдкар без проброшенных портов; контейнер от root.
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    PLAYWRIGHT_BROWSERS_PATH=/ms-browsers \
    DISPLAY=:99

WORKDIR /app

COPY requirements.txt .
RUN pip install -r requirements.txt \
 && patchright install --with-deps chromium \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip

# Отдельным слоем (после Chromium, чтобы не сбивать кэш): Xvfb, программный GL,
# кириллические шрифты.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb libgl1-mesa-dri fonts-liberation fonts-dejavu-core \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh .
COPY server.py .
RUN chmod +x entrypoint.sh

ENTRYPOINT ["./entrypoint.sh"]
