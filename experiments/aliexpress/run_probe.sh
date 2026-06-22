#!/usr/bin/env bash
# Ставит Camoufox в изолированный venv и запускает probe.py.
# Запускать НА VPS (чистый датацентр-IP, без vless) из корня репо или из этой папки.
#
#   bash experiments/aliexpress/run_probe.sh
#   ALI_ID=1005005863682926 bash experiments/aliexpress/run_probe.sh
#
# Системные зависимости (один раз, нужен sudo) — Firefox + Xvfb для virtual-headless:
#   sudo apt-get update && sudo apt-get install -y \
#     libgtk-3-0 libx11-xcb1 libasound2 libdbus-glib-1-2 libxtst6 libxt6 xvfb
#
# Если xvfb ставить не хочешь — запусти с HEADLESS_MODE=true (нативный headless):
#   HEADLESS_MODE=true bash experiments/aliexpress/run_probe.sh
set -euo pipefail

HERE="$(cd "$(dirname "$0")" && pwd)"
cd "$HERE"

# 1. uv (самодостаточный менеджер пакетов/venv, без sudo)
if ! command -v uv >/dev/null 2>&1; then
  export PATH="$HOME/.local/bin:$PATH"
fi
if ! command -v uv >/dev/null 2>&1; then
  echo "== ставлю uv =="
  curl -LsSf https://astral.sh/uv/install.sh | sh
  export PATH="$HOME/.local/bin:$PATH"
fi

# 2. venv + camoufox
if [ ! -d camoenv ]; then
  echo "== создаю venv =="
  uv venv --python 3.12 camoenv
fi
# shellcheck disable=SC1091
. camoenv/bin/activate
echo "== ставлю camoufox =="
uv pip install -q "camoufox[geoip]"

# 3. патченый Firefox (идемпотентно, кэшируется)
echo "== fetch браузера camoufox =="
python -m camoufox fetch

# 4. запуск проба
echo "== запуск проба =="
export OUT_DIR="${OUT_DIR:-/tmp/aliprobe}"
exec python probe.py
