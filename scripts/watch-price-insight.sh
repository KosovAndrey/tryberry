#!/usr/bin/env bash
# Наблюдение за price-insight в первые часы после выкатки: пишет в лог память
# контейнера, суммарный лаг группы и число строк в price_insight.
#
# Главный вопрос первых часов — выйдет ли память на полку. При выкатке 13.08 она
# выросла с 267 МБ до 460 МБ, пока сервис жевал бэклог (578 тыс. событий за 72ч
# retention); лимит контейнера 1 ГБ, и упереться в него значит быть убитым
# OOM-killer'ом. Стейт RocksDB ограничен сверху (BoundedMemoryRocksDBConfig), но
# смета составлена по умолчаниям, а не по замеру — вот и замеряем.
#
# Запуск в фоне на проде (переживает выход из ssh):
#   nohup ./scripts/watch-price-insight.sh > /dev/null 2>&1 &
#   tail -f ~/price-insight-watch.log
#
# Остановить: pkill -f watch-price-insight
set -u

LOG="${LOG:-$HOME/price-insight-watch.log}"
INTERVAL="${INTERVAL:-300}"
C="docker compose -f docker-compose.yml -f docker-compose.prod.yml"

echo "== наблюдение начато $(date -Is), интервал ${INTERVAL}с ==" >> "$LOG"

while true; do
    ts=$(date -Is)

    mem=$(docker stats --no-stream --format '{{.MemUsage}} cpu={{.CPUPerc}}' \
          pt_price_insight 2>/dev/null | tr -d '\n')
    [ -z "$mem" ] && mem="контейнер не отвечает"

    # Лаг суммируем только по числовым значениям: под exactly_once у партиций без
    # закоммиченного оффсета в колонке стоит «-», а не число.
    lag=$($C exec -T kafka kafka-consumer-groups --bootstrap-server localhost:9092 \
              --describe --group price-insight 2>/dev/null \
          | awk 'NR>1 && $6 ~ /^[0-9]+$/ {s+=$6} END {print s+0}')
    [ -z "$lag" ] && lag="?"

    rows=$($C exec -T postgres psql -U user -d tryberrybot -At \
               -c 'SELECT count(*) FROM price_insight' 2>/dev/null | tr -d '\r')
    [ -z "$rows" ] && rows="?"

    # Живость потока: healthy_lanes-подобной ловушки тут нет, но состояние
    # Streams-потока показывает, не умер ли он тихо.
    state=$($C exec -T price-insight wget -qO- \
                http://localhost:8092/actuator/health 2>/dev/null | tr -d '\n')
    [ -z "$state" ] && state="health недоступен"

    echo "$ts mem=$mem lag=$lag rows=$rows health=$state" >> "$LOG"
    sleep "$INTERVAL"
done
