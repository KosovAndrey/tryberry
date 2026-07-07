#!/usr/bin/env bash
# wb-basket-probe — measure the real WB CDN basket shard for high-vol nmIDs,
# to calibrate wbBasketNumber() in internal/scraper/wildberries.go.
#
# WHY bash+curl and not Go: the WB search (search.wb.ru) sits behind the wbaas
# antibot, which challenges Go's net/http.Client by JA3/HTTP2 fingerprint and
# returns 429. curl's TLS fingerprint passes for cold queries. The basket CDN
# (basket-*.wbbasket.ru) has NO antibot — it plainly returns 200 on the correct
# shard and 404 on every other — so any client works there.
#
# Pipeline:
#   1) harvest — pull real, existing nmIDs from search (sort=newly returns a wide
#      spread of vols in a single page), bucket first-seen id per vol.
#   2) probe   — for each id scan basket-NN until card.json returns 200.
#
# Usage:
#   ./probe.sh harvest "запрос1" "запрос2" ...   # -> ids.txt (vol id)
#   ./probe.sh probe   [ids.txt]                 # -> stdout "vol id basket=N"
#   ./probe.sh all                               # harvest a default query set + probe
#
# Be polite: <=5 rps, keep the query list small/cold; hammering search trips a
# transient per-IP 429 (wait a few minutes to recover).
set -u

UA='Mozilla/5.0 (X11; Linux x86_64) AppleWebKit/537.36 (KHTML, like Gecko) Chrome/124.0.0.0 Safari/537.36'
SEARCH='https://search.wb.ru/exactmatch/ru/common/v9/search'
DIR="$(cd "$(dirname "$0")" && pwd)"
IDS="$DIR/ids.txt"

urlenc() { python3 -c 'import sys,urllib.parse;print(urllib.parse.quote(sys.argv[1]))' "$1"; }

harvest() {
  : > "$IDS.raw"
  for q in "$@"; do
    for sort in newly popular; do
      url="$SEARCH?appType=1&curr=rub&dest=-1257786&query=$(urlenc "$q")&resultset=catalog&sort=$sort"
      curl -s -m 15 "$url" -H "User-Agent: $UA" \
        | python3 -c 'import sys,json
try: d=json.load(sys.stdin)
except Exception: sys.exit(0)
for x in d.get("products",[]): print(x["id"]//100000, x["id"])' >> "$IDS.raw"
      sleep 4   # cold-query pacing to stay under the antibot radar
    done
  done
  # first-seen id per vol, sorted by vol
  sort -n -k1 "$IDS.raw" | awk '!seen[$1]++' > "$IDS"
  rm -f "$IDS.raw"
  echo "harvested $(wc -l < "$IDS") vols -> $IDS" >&2
}

probe() {
  local file="${1:-$IDS}"
  while read -r vol id; do
    [ -z "$vol" ] && continue
    part=$((id/1000)); found=""
    for b in $(seq 28 52); do
      code=$(curl -s -m 6 "https://basket-$(printf %02d "$b").wbbasket.ru/vol$vol/part$part/$id/info/ru/card.json" -o /dev/null -w "%{http_code}")
      if [ "$code" = "200" ]; then found=$b; break; fi
      sleep 0.2   # ~5 rps on the CDN
    done
    echo "$vol $id basket=$found"
  done < "$file"
}

case "${1:-all}" in
  harvest) shift; harvest "$@" ;;
  probe)   shift; probe "$@" ;;
  all)
    harvest капибара пряжа термос гамак телескоп мангал бинокль секатор
    probe
    ;;
  *) echo "usage: $0 {harvest <queries...>|probe [ids.txt]|all}" >&2; exit 2 ;;
esac
