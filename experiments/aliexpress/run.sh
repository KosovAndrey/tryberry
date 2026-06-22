#!/usr/bin/env bash
# Phase-1 проб aliexpress.ru — собирает ОТДЕЛЬНЫЙ образ ali-probe (Camoufox+Xvfb,
# без Ozon, без прокси) и запускает его. Запускать НА VPS (чистый датацентр-IP).
#
#   bash experiments/aliexpress/run.sh
#   ALI_ID=1005005863682926 bash experiments/aliexpress/run.sh
#   ALI_PROXY=http://user:pass@host:port bash experiments/aliexpress/run.sh   # если без прокси не пускает
#
# Артефакты (page.png/html, resp_*.json) появятся в experiments/aliexpress/out/.
set -euo pipefail
ROOT="$(git rev-parse --show-toplevel 2>/dev/null || cd "$(dirname "$0")/../.." && pwd)"
DIR="$ROOT/experiments/aliexpress"

echo "== сборка образа ali-probe =="
docker build -t ali-probe "$DIR"

OUT="$DIR/out"
mkdir -p "$OUT"

echo "== запуск проба (proxy: ${ALI_PROXY:+есть}${ALI_PROXY:-НЕТ}) =="
# Без --network = дефолтный bridge = датацентр-IP VPS (ровно то, что тестируем).
# Прокси задаётся ТОЛЬКО через ALI_PROXY (по умолчанию Ali ходит напрямую).
# ali_probe.py запечён в образ; монтируем поверх для быстрых правок без ребилда.
exec docker run --rm \
  --shm-size=1gb \
  -e ALI_ID="${ALI_ID:-1005005863682926}" \
  -e ALI_PROBE_URL="${ALI_PROBE_URL:-}" \
  -e ALI_PROXY="${ALI_PROXY:-}" \
  -e OUT_DIR=/tmp/aliprobe \
  -e HEADLESS="${HEADLESS:-false}" \
  -e LOG_LEVEL="${LOG_LEVEL:-INFO}" \
  -v "$DIR/ali_probe.py:/app/ali_probe.py:ro" \
  -v "$OUT:/tmp/aliprobe" \
  ali-probe
