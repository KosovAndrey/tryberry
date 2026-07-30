#!/usr/bin/env bash
# Линейка задержки long-poll: считает интервалы между getUpdates по логам xray.
#
# Зачем: алерт TelegramPollingStale ловит только «поллинг умер совсем», а
# деградацию «бот отвечает не за 2 секунды, а за 8» не видит никто. Интервал
# между запросами = TELEGRAM_POLL_TIMEOUT_SECONDS + накладные на плечо, поэтому
# «накладные» из вывода — это прямая цена текущего egress.
#
#   scripts/poll-stats.sh          # окно 3 минуты
#   scripts/poll-stats.sh 10m      # окно 10 минут
#
# Как читать: накладные до ~0.5с — здоровое плечо. Секунды — плечо медленное
# или роняет удерживаемые соединения; отличить одно от другого можно по
# tryberrybot_telegram_poll_errors_total (растёт = рвётся, стоит = просто далеко).
# Ориентир для «рвётся»: провалившийся полл стоит Client.Timeout (poll+3) плюс
# errorBackoff 3с, так что max ≈ poll+6 — это ретрай, а не задержка сети.
set -euo pipefail

since="${1:-3m}"
poll_timeout="$(docker exec pt_api printenv TELEGRAM_POLL_TIMEOUT_SECONDS 2>/dev/null || true)"
poll_timeout="${poll_timeout:-10}"   # дефолт из docker-compose.yml

docker logs pt_xray --since "$since" 2>&1 \
| grep 'api.telegram.org' \
| awk -v base="$poll_timeout" '
    # Формат строки xray: 2026/07/31 05:14:02.094071 from ... accepted //api.telegram.org:443 [in -> ТЕГ]
    { split($2, c, ":"); t = c[1]*3600 + c[2]*60 + c[3]
      if (match($0, /\[in -> [^]]+\]/)) leg[substr($0, RSTART+7, RLENGTH-8)]++
      if (prev && t > prev) { d = t - prev; n++; sum += d
                              if (!min || d < min) min = d
                              if (d > max) max = d }
      prev = t }
    END {
      if (!n) { print "нет данных: за окно ни одного запроса к Telegram"; exit 1 }
      printf "циклов %d  мин %.2f  среднее %.2f  макс %.2f  → накладные %.2fс (long-poll %ds)\n",
             n, min, sum/n, max, sum/n - base, base
      printf "плечи:"; for (l in leg) printf " %s×%d", l, leg[l]; print ""
    }'
