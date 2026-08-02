#!/usr/bin/env bash
# Черновая озвучка сборного ролика V2 по битам (docs/content/batch-03.md).
#
# По биту на файл, а не одной дорожкой: на монтаже каждую фразу двигают
# отдельно, подгоняя под план. Склеенный трек кладётся рядом — им удобно
# сверять общий хронометраж.
#
# Голос ru-RU-DmitryNeural и темп чуть выше обычного — канон tryberry-shorts.
# Это ЧЕРНОВИК под монтаж; финальную озвучку писать в ElevenLabs.
#
# Требует: sudo apt install -y python3-pip python3-venv
set -euo pipefail

cd "$(dirname "$0")"
OUT="${1:-out}"
VOICE="${VOICE:-ru-RU-DmitryNeural}"
RATE="${RATE:-+10%}"

if [ ! -x .venv/bin/edge-tts ]; then
  echo "Ставлю edge-tts в .venv…"
  python3 -m venv .venv
  .venv/bin/pip install --quiet --upgrade pip edge-tts
fi

mkdir -p "$OUT"
: > "$OUT/concat.txt"

while IFS='|' read -r id text; do
  [ -z "${id:-}" ] && continue
  .venv/bin/edge-tts --voice "$VOICE" --rate "$RATE" --text "$text" --write-media "$OUT/$id.mp3"
  dur=$(/usr/bin/ffprobe -v error -show_entries format=duration -of csv=p=0 "$OUT/$id.mp3")
  printf "%s  %5.2f с  %s\n" "$id" "$dur" "${text:0:52}…"
  echo "file '$id.mp3'" >> "$OUT/concat.txt"
done < v2-lines.txt

ffmpeg -v error -y -f concat -safe 0 -i "$OUT/concat.txt" -c copy "$OUT/vo-full.mp3"
echo "─────"
printf "весь трек: %.2f с\n" "$(/usr/bin/ffprobe -v error -show_entries format=duration -of csv=p=0 "$OUT/vo-full.mp3")"
