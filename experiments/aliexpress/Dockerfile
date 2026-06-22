# ── ali-probe ─────────────────────────────────────────────────────────────
# Отдельный образ под AliExpress (НЕ переиспользуем ozon-miner: у Ozon свой
# proxy/cookie/lane-код, а Ali ходит БЕЗ прокси с датацентр-IP). Здесь только
# Camoufox + Xvfb — ровно для Phase-1 проба; дальше из него вырастет сайдкар
# ali-miner (aiohttp + пул дорожек), по образцу ozon-miner, но без прокси.
#
# Сборка/запуск — через experiments/aliexpress/run.sh (он сам билдит образ).
FROM python:3.12-slim

ENV PYTHONUNBUFFERED=1 \
    PIP_NO_CACHE_DIR=1 \
    DISPLAY=:99

WORKDIR /app

# camoufox = анти-детект Firefox (проходит X5SEC, как FAB у Ozon). geoip-extra
# выравнивает локаль/таймзону под exit-IP. playwright (тянется camoufox) даёт CLI
# install-deps для системных либ Firefox (GTK/X11/звук) — то, об что спотыкается
# голый venv на хосте. Этот слой кэшируется.
RUN pip install "camoufox[geoip]" \
 && python -m camoufox fetch \
 && playwright install-deps firefox \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/* /root/.cache/pip

# Xvfb (headful палится), программный GL (Mesa, на VPS без GPU) и кириллица.
RUN apt-get update \
 && apt-get install -y --no-install-recommends \
      xvfb libgl1-mesa-dri fonts-liberation fonts-dejavu-core \
 && apt-get clean \
 && rm -rf /var/lib/apt/lists/*

COPY ali_probe.py .

ENTRYPOINT ["python", "ali_probe.py"]
