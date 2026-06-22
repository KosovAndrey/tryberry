#!/usr/bin/env bash
# Phase-1 проб aliexpress.ru — запуск В ОБРАЗЕ ozon-miner (там уже camoufox+Xvfb+
# все OS-либы, поэтому никаких ручных apt/libgtk/xvfb на хосте). Эквивалент того,
# как ozon-miner/README запускает probe.py. Запускать НА VPS (чистый датацентр-IP).
#
#   bash experiments/aliexpress/run.sh
#   ALI_ID=1005005863682926 bash experiments/aliexpress/run.sh
#   ALI_PROXY=http://user:pass@host:port bash experiments/aliexpress/run.sh   # если без прокси не пускает
#
# Артефакты (page.png/html, resp_*.json) появятся в experiments/aliexpress/out/.
set -euo pipefail
cd "$(git rev-parse --show-toplevel 2>/dev/null || dirname "$(dirname "$0")")"

# Образ с camoufox: явный ALI_PROBE_IMAGE или авто-поиск собранного miner-образа.
IMAGE="${ALI_PROBE_IMAGE:-}"
if [ -z "$IMAGE" ]; then
  IMAGE="$(docker images --format '{{.Repository}}:{{.Tag}}' \
           | grep -iE 'ozon.?miner|token.?miner' | grep -v '<none>' | head -1 || true)"
fi
if [ -z "$IMAGE" ]; then
  echo "Не нашёл собранный образ camoufox (ozon-miner/token-miner)."
  echo "Собери его:  docker compose build ozon-miner"
  echo "или укажи:   ALI_PROBE_IMAGE=<image> bash experiments/aliexpress/run.sh"
  exit 1
fi
echo "образ: $IMAGE"

OUT="$PWD/experiments/aliexpress/out"
mkdir -p "$OUT"

# --shm-size: Firefox любит большой /dev/shm. Без --network = дефолтный bridge =
# датацентр-IP VPS (ровно то, что тестируем). Прокси — только если задан ALI_PROXY.
exec docker run --rm \
  --shm-size=1gb \
  -e ALI_ID="${ALI_ID:-1005005863682926}" \
  -e ALI_PROBE_URL="${ALI_PROBE_URL:-}" \
  -e ALI_PROXY="${ALI_PROXY:-}" \
  -e OUT_DIR=/tmp/aliprobe \
  -e HEADLESS="${HEADLESS:-false}" \
  -e LOG_LEVEL="${LOG_LEVEL:-INFO}" \
  -v "$PWD/experiments/aliexpress/ali_probe.py:/app/ali_probe.py:ro" \
  -v "$OUT:/tmp/aliprobe" \
  --entrypoint python \
  "$IMAGE" /app/ali_probe.py
