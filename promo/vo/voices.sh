#!/usr/bin/env bash
# Прослушивание голосов: одна фраза во ВСЕХ голосах, умеющих русский.
#
# Зачем отдельно от make-vo.sh: гонять полные 45 секунд в десяти голосах долго
# и незачем — выбор слышен на первой фразе. Выбрали → make-vo.sh с VOICE=.
#
# Берём две группы: штатные ru-RU (их мало) и мультиязычные Multilingual —
# последние читают русский и звучат заметно живее, ради них всё и затевалось.
#
# Требует: sudo apt install -y python3-pip python3-venv
set -euo pipefail

cd "$(dirname "$0")"
OUT="${1:-out-samples}"
RATE="${RATE:-+10%}"
# Хук ролика: по нему и слышно, «свой человек» или диктор.
TEXT="${TEXT:-Залипаешь на маркетплейсах? Тогда знаешь это чувство: вещь отличная, цена — не очень.}"

if [ ! -x .venv/bin/edge-tts ]; then
  echo "Ставлю edge-tts в .venv…"
  python3 -m venv .venv
  .venv/bin/pip install --quiet --upgrade pip edge-tts
fi

mkdir -p "$OUT"

VOICES=$(.venv/bin/edge-tts --list-voices \
  | awk '{print $1}' \
  | grep -E '^(ru-RU-|.*Multilingual)' \
  | sort -u)

echo "Голосов к прослушиванию: $(echo "$VOICES" | wc -l)"
echo

for v in $VOICES; do
  f="$OUT/${v}.mp3"
  if .venv/bin/edge-tts --voice "$v" --rate "$RATE" --text "$TEXT" --write-media "$f" 2>/dev/null; then
    dur=$(/usr/bin/ffprobe -v error -show_entries format=duration -of csv=p=0 "$f" 2>/dev/null || echo 0)
    printf "  %-42s %5.2f с\n" "$v" "$dur"
  else
    printf "  %-42s — не отдал русский\n" "$v"
    rm -f "$f"
  fi
done

echo
echo "Слушать: $OUT/"
echo "Выбрали → VOICE=<имя> ./make-vo.sh out-<имя>"
