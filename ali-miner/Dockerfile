# ── Ali miner (браузер-как-транспорт для поисковой выдачи aliexpress.ru) ──────
# python:3.12-slim + Chromium (patchright) + Xvfb. Тот же движок и приём, что у
# wb-search-miner (headful в Xvfb проходит антибот), но под X5SEC у Ali: сервис
# долгоживущий, держит прогретые дорожки и отдаёт выдачу /aer-webapi/v1/search
# перехватом нативного XHR по HTTP (aiohttp). Внутренний сайдкар без портов
# наружу; контейнер от root.
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
