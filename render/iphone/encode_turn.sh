#!/usr/bin/env bash
# Сборка ВСЕХ веб-ассетов hero из одной рендер-сессии (out/ после turn.py):
#   web/hero-iphone-turn.webm         — альфа-WebM разворота (turn_####.png)
#   web/hero-iphone-turn-poster.webp  — постер (кадр 1, крышка)
#   web/hero-iphone.png / .webp       — стилл (out/still_front.png, кадр == последний кадр видео)
#   web/hero-gloss.webp               — кроп дисплея из стилла по out/screen_rect.json
#                                       + зачистка зоны острова (камера/динамик не просвечивают)
# Запускать в WSL; путь к out/turn Windows-клона можно передать первым аргументом:
#   bash render/iphone/encode_turn.sh /mnt/c/Users/.../tryberrybot/render/iphone/out/turn
# (still_front.png и screen_rect.json ищутся рядом, в родителе turn/)
#
# Альфа в VP9 требует -auto-alt-ref 0 (иначе libvpx молча выкидывает альфу).
# Safari VP9-альфу не умеет — на сайте фолбэк: сразу статичный PNG без интро.
# ffprobe из ~/.local/bin сегфолтится — зовём бинари по полному пути /usr/bin.
set -euo pipefail
SDIR="$(cd "$(dirname "$0")" && pwd)"
SEQ_DIR="${1:-$SDIR/out/turn}"
OUT_DIR="$(dirname "$SEQ_DIR")"
WEB="$SDIR/../../web"
FPS="${FPS:-60}"   # держать = turn_fps в scene.py
CRF="${CRF:-32}"   # 28 качественнее/тяжелее, 36 легче
FF=/usr/bin/ffmpeg

[ -e "$SEQ_DIR/turn_0001.png" ] || { echo "нет $SEQ_DIR/turn_0001.png — сначала рендер в Blender (turn.py)"; exit 1; }

echo "── 1/4 альфа-WebM разворота"
"$FF" -y -framerate "$FPS" -i "$SEQ_DIR/turn_%04d.png" \
  -c:v libvpx-vp9 -pix_fmt yuva420p -b:v 0 -crf "$CRF" \
  -auto-alt-ref 0 -row-mt 1 \
  "$WEB/hero-iphone-turn.webm"

echo "── 2/4 постер (кадр 1, крышка)"
"$FF" -y -v error -i "$SEQ_DIR/turn_0001.png" -c:v libwebp -quality 82 -pix_fmt yuva420p \
  "$WEB/hero-iphone-turn-poster.webp"

STILL="$OUT_DIR/still_front.png"
RECT="$OUT_DIR/screen_rect.json"
if [ ! -e "$STILL" ] || [ ! -e "$RECT" ]; then
  echo "!! нет $STILL или $RECT — turn.py теперь рендерит их сам (нужен свежий scene.py)."
  echo "!! WebM/постер собраны, но стилл/gloss НЕ обновлены — стык может уехать!"
  exit 1
fi

echo "── 3/4 стилл: hero-iphone.png / .webp"
cp "$STILL" "$WEB/hero-iphone.png"
"$FF" -y -v error -i "$STILL" -c:v libwebp -quality 82 -pix_fmt yuva420p "$WEB/hero-iphone.webp"

echo "── 4/4 gloss: кроп дисплея по screen_rect.json + зачистка зоны острова"
# Зона остров+камера в ДОЛЯХ дисплея (замер 2026-07-11 по старому кропу 641×1388:
# юнит x[231,411] y[20,71] + поля) — геометрия камеры/модели не менялась, доли валидны.
CROP_ARGS=$(python3 - "$RECT" <<'EOF'
import json, sys
r = json.load(open(sys.argv[1]))
rx, ry = r["res_x"], r["res_y"]
x = round(r["left"] / 100 * rx); y = round(r["top"] / 100 * ry)
w = round(r["width"] / 100 * rx); h = round(r["height"] / 100 * ry)
# остров: доли от кропа дисплея
ix0, ix1 = round(0.3432 * w), round(0.6583 * w)
iy0, iy1 = round(0.0072 * h), round(0.0591 * h)
print(f"{w}:{h}:{x}:{y} {ix0} {iy0} {ix1 - ix0} {iy1 - iy0}")
print(f"CSS .phone-screen: left:{r['left']}%;top:{r['top']}%;width:{r['width']}%;height:{r['height']}%", file=sys.stderr)
EOF
)
read -r CROP IX IY IW IH <<< "$CROP_ARGS"
"$FF" -y -v error -i "$STILL" \
  -vf "crop=$CROP,drawbox=x=$IX:y=$IY:w=$IW:h=$IH:color=black@1:t=fill" \
  -c:v libwebp -lossless 1 "$WEB/hero-gloss.webp"

ls -la "$WEB/hero-iphone-turn.webm" "$WEB/hero-iphone-turn-poster.webp" \
       "$WEB/hero-iphone.png" "$WEB/hero-iphone.webp" "$WEB/hero-gloss.webp"
echo
echo "OK: все ассеты из одной сессии. Дальше в web/index.html:"
echo "  1) .phone-screen ← проценты из $RECT (left/top/width/height, печатались выше)"
echo "  2) УБРАТЬ CSS-фадж transform у .phone-turn (стилл теперь == последний кадр видео)"
echo "  3) сверить .phone-notch по новому gloss (доли острова не должны были уехать)"
