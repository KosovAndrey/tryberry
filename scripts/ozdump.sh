#!/usr/bin/env bash
# ozdump.sh — снять реальный ответ Ozon с живого сайдкара (pt_ozon_miner) для
# тюнинга маркеров 18+/login и диагностики парсинга.
#
# Использование (на ВПС, из ~/projects/tryberrybot):
#   bash scripts/ozdump.sh '<url-товара>' ['<url-товара2>' ...]
#
# Можно передавать как полные URL карточки, так и просто числовой id.
# Полный сырой widgetStates каждого товара ложится в /tmp/ozon_<id>.json,
# а в терминал печатается компактная сводка: имена виджетов, есть ли цена (₽),
# и совпадения по маркерам возраста/логина. Сводку пришли Клоду.
set -euo pipefail

CONTAINER="${OZON_MINER_CONTAINER:-pt_ozon_miner}"

if [ "$#" -eq 0 ]; then
  echo "использование: bash scripts/ozdump.sh '<url-или-id>' ['<url-или-id>' ...]" >&2
  exit 1
fi

for arg in "$@"; do
  # id = последняя группа цифр в /product/...-<id> либо аргумент целиком, если это число
  id=$(printf '%s' "$arg" | sed -E 's#.*/product/(.*-)?([0-9]+).*#\2#')
  if ! printf '%s' "$id" | grep -qE '^[0-9]+$'; then
    id="$arg"
  fi
  if ! printf '%s' "$id" | grep -qE '^[0-9]+$'; then
    echo "пропускаю '$arg': не нашёл числовой id" >&2
    continue
  fi

  docker exec -i "$CONTAINER" python3 - "$id" <<'PY'
import json, sys, urllib.request, urllib.error
pid = sys.argv[1]
try:
    raw = urllib.request.urlopen('http://localhost:8080/scrape?id=%s' % pid, timeout=90).read().decode()
except urllib.error.HTTPError as e:
    raw = e.read().decode()
except Exception as ex:
    print('=== id', pid, '— ЗАПРОС УПАЛ:', str(ex)[:200], '==='); sys.exit()
open('/tmp/ozon_%s.json' % pid, 'w').write(raw)
try:
    ws = json.loads(raw).get('widgetStates', {})
except Exception as ex:
    print('=== id', pid, '— НЕ JSON:', str(ex)[:120], '==='); print(raw[:400]); sys.exit()
has_rub = any('₽' in v for v in ws.values())
print('=== id', pid, '— widgets:', len(ws), '| has ₽:', has_rub, '===')
# Помечаем, ЧТО несёт каждый виджет: ₽ = цена, IMG = продуктовая картинка CDN.
# Это и есть источники «чужой» цены/фото у OOS-товаров (виджеты полок «с этим
# покупают» тоже несут ₽ и multimedia — парсер обязан их отличать от карточки).
for k in sorted(ws):
    v = ws[k]
    flags = []
    if '₽' in v:
        flags.append('₽')
    if '/multimedia' in v:
        flags.append('IMG')
    print('   %-46s %s' % (k, ' '.join(flags)))
print('  --- виджеты с ₽ (сниппет вокруг первой цены) ---')
for k in sorted(ws):
    v = ws[k]
    if '₽' not in v:
        continue
    i = v.find('₽')
    print('   [%s] …%s…' % (k, v[max(0, i-160):i+40].replace(chr(10), ' ')))
print('  --- виджеты с продуктовой картинкой ---')
for k in sorted(ws):
    v = ws[k]
    if '/multimedia' not in v:
        continue
    i = v.find('/multimedia')
    print('   [%s] …%s…' % (k, v[max(0, i-60):i+60].replace(chr(10), ' ')))
kws = ['adult','ageverif','age_verif','возраст','взросл','вам есть 18','вам уже есть 18','18+',
       'войти','войдите','авториз','signin','login']
hits = 0
for k, v in ws.items():
    lk, lv = k.lower(), v.lower()
    for kw in kws:
        if kw in lk:
            print('  MARKER(name) [%s] kw=%s' % (k, kw)); hits += 1
        elif kw in lv:
            i = lv.find(kw)
            sn = v[max(0, i-60):i+120].replace(chr(10), ' ')
            print('  MARKER(val)  [%s] kw=%s …%s…' % (k, kw, sn)); hits += 1
print('  marker hits:', hits)
print('  (полный дамп: /tmp/ozon_%s.json)' % pid)
PY
done
