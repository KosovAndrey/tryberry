#!/usr/bin/env bash
# Энкод PNG-секвенции разворота (out/turn/turn_####.png, RGBA) в альфа-WebM
# для hero сайта. Запускать в WSL из любого места: сам найдёт свои пути.
#
# Кадры рендерятся на Windows (Blender, CONFIG["turn"]=True) в Windows-клоне
# репо; путь к out/turn можно передать первым аргументом:
#   bash render/iphone/encode_turn.sh /mnt/c/Users/.../tryberrybot/render/iphone/out/turn
#
# Альфа в VP9 требует -auto-alt-ref 0 (иначе libvpx молча выкидывает альфу).
# Safari VP9-альфу не умеет — на сайте фолбэк: сразу статичный PNG без интро.
set -euo pipefail
SDIR="$(cd "$(dirname "$0")" && pwd)"
SEQ_DIR="${1:-$SDIR/out/turn}"
FPS="${FPS:-30}"
CRF="${CRF:-32}"      # 28 качественнее/тяжелее, 36 легче
OUT="$SDIR/../../web/hero-iphone-turn.webm"

[ -e "$SEQ_DIR/turn_0001.png" ] || { echo "нет $SEQ_DIR/turn_0001.png — сначала рендер в Blender"; exit 1; }

ffmpeg -y -framerate "$FPS" -i "$SEQ_DIR/turn_%04d.png" \
  -c:v libvpx-vp9 -pix_fmt yuva420p -b:v 0 -crf "$CRF" \
  -auto-alt-ref 0 -row-mt 1 \
  "$OUT"

ls -la "$OUT"
echo "OK: web/hero-iphone-turn.webm — дальше подключаем на сайте (видео → PNG → DOM-экран)"
