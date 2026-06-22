#!/usr/bin/env bash
# Phase-1: собрать образ ali-miner и прогнать проб X5SEC на aliexpress.ru.
# Запускать НА VPS (чистый датацентр-IP, без vless).
#
#   bash ali-miner/run.sh
#   ALI_ID=1005005863682926 bash ali-miner/run.sh
#   ALI_PROXY=http://user:pass@host:port bash ali-miner/run.sh   # если без прокси не пускает
#
# Артефакты (page.png/html, resp_*.json) появятся в ali-miner/out/.
set -euo pipefail
DIR="$(cd "$(dirname "$0")" && pwd)"

echo "== сборка образа ali-miner =="
docker build -t ali-miner "$DIR"

OUT="$DIR/out"
mkdir -p "$OUT"

echo "== запуск проба (proxy: ${ALI_PROXY:-НЕТ}) =="
# Без --network = дефолтный bridge = датацентр-IP VPS (то, что тестируем).
# Прокси — ТОЛЬКО если задан ALI_PROXY. CMD образа = python probe.py.
exec docker run --rm \
  --shm-size=1gb \
  -e ALI_ID="${ALI_ID:-1005005863682926}" \
  -e ALI_PROBE_URL="${ALI_PROBE_URL:-}" \
  -e ALI_PROXY="${ALI_PROXY:-}" \
  -e OUT_DIR=/tmp/aliprobe \
  -e HEADLESS="${HEADLESS:-false}" \
  -e LOG_LEVEL="${LOG_LEVEL:-INFO}" \
  -v "$OUT:/tmp/aliprobe" \
  ali-miner
