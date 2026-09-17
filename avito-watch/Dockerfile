# ── avito-watch (личный мониторинг новых объявлений Авито в Telegram) ─────────
# Тот же движок, что у ali-miner: python:3.12-slim + Chromium (patchright) +
# Xvfb, headful проходит антибот. Портов наружу нет.
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

RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb libgl1-mesa-dri fonts-liberation fonts-dejavu-core \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh extract.py watch.py ./
RUN chmod +x entrypoint.sh

HEALTHCHECK --interval=2m --timeout=10s --start-period=2m \
  CMD python -c "import time,sys; sys.exit(time.time()-float(open('/data/heartbeat').read())>900)"

ENTRYPOINT ["./entrypoint.sh"]
