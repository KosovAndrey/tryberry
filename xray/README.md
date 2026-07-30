# Xray (VLESS) — единственный egress в Telegram

На RU-хостинге РКН душит Telegram. Раньше весь egress шёл через WireGuard
(`wg-proxy` + `tinyproxy`), но WG — это UDP, и DPI его прицельно троттлит →
зависания long-poll на десятки секунд. VLESS идёт по TCP под видом обычного
HTTPS (особенно **VLESS+Reality** — маскируется под TLS к чужому сайту), и
почти не троттлится. WG-резерв выпилен 2026-07-11: handshake перестал
проходить совсем (при живом хосте — DPI), а unhealthy `wg-proxy` обрывал
каждый полный `up -d`. История: git log по `wireguard/`.

## Как это устроено

Один контейнер `xray` поднимает локальный HTTP-прокси на `:8888`. Плечи выхода —
outbound'ы `vless-*` (по умолчанию 16 узлов подписки, разложенных по странам).

`observatory` каждые 30с пробит все плечи до `api.telegram.org`, `balancer`
(`leastPing`) держит трафик на **живом и самом быстром** узле. Узел отвалился —
автоматически уходим на следующий, без рестартов. Селектор балансировщика
матчит теги по префиксу, поэтому `["vless"]` охватывает все плечи сразу.

Сервисы `api`/`bot-worker`/`notifier` ходят сюда как
`HTTPS_PROXY=http://xray:8888`.

## Конфиг собирается из подписки скриптом

Xray-core **не умеет подписочные ссылки** — их разворачивают клиенты
(v2rayN/Nekobox), ядро ест только статический `config.json`. Пока узлов было
шесть, их вписывали руками; на подписке в ~80 узлов это неподъёмно, поэтому
конфиг генерится:

```bash
python3 scripts/xray-config-from-sub.py 'https://ПОДПИСКА/КОД' -o xray/config.json
```

Скрипт разворачивает `vless://`-ссылки в outbound'ы, вешает на них общий
balancer и observatory — то же, что было руками, только воспроизводимо. Полезные
ключи:

| Ключ                 | Зачем                                                    |
|----------------------|----------------------------------------------------------|
| `--list`             | показать узлы подписки и выйти, ничего не записывая        |
| `-n 24`              | сколько плеч оставить (по умолчанию 16, `0` = все)         |
| `--include 'Финлян'` | оставить только узлы, чьё имя/хост совпали с regex         |
| `--exclude 'Турц'`   | выкинуть узлы по regex (матчит имя и хост, не транспорт)   |
| `--network tcp`      | только этот транспорт — под long-poll лучший `tcp`+vision  |

Все узлы брать не надо: observatory пробит **каждое** плечо раз в 30с, и сотня
лишних коннектов в минуту ради узлов в Канаде egress'у не помогает. Скрипт
набирает плечи round-robin по странам ближнего круга (FI, EE, LV, LT, SE, NL,
DE, PL), так что падение одной локации не выкашивает все плечи разом; дальние
страны идут только на добор.

Поддержаны reality поверх `tcp`, `grpc` и `xhttp`. `flow=xtls-rprx-vision`
проставляется только на `tcp` — на grpc/xhttp ядро с ним не стартует.

> `xray/config.json` — **секрет** (внутри UUID и pbk), в `.gitignore`. В git
> лежит только `config.json.example` с плейсхолдерами. Сама ссылка подписки в
> репозиторий тоже не попадает.

Поднять и перецепить потребителей прокси:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d xray
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d api bot-worker notifier
```

Сменились узлы у провайдера — перегенерить тем же скриптом и `up -d xray`.

## Проверка

```bash
# Через какое плечо реально идёт трафик: ищем [in -> vless-XXX]:
docker compose logs --tail=30 xray

# Какой IP видит мир через xray (в образе busybox wget — прокси через env + -Y on):
docker compose exec xray sh -c "http_proxy=http://127.0.0.1:8888 wget -Y on -qO- http://api.ipify.org; echo"

# Доходит ли до Telegram (главное, ради чего всё):
docker compose exec xray sh -c "http_proxy=http://127.0.0.1:8888 wget -Y on -qO- -T 15 https://api.telegram.org/; echo RC=\$?"
```

Если все `vless-*` помечаются мёртвыми (Telegram перестал отвечать):
- `x509: certificate is valid for ... not <SNI>` → `serverName` не тот, что ждёт
  сервер: перегенерь конфиг из **свежей** подписки (провайдер сменил камуфляж);
- `REALITY: processed invalid connection` → `pbk`/`sid` разошлись с подпиской;
- плечи живы, но long-poll всё равно висит → проблема не в xray, см. алерт
  `TelegramPollingStale` и `telegram_poll_errors_total`: если счётчик ошибок
  **не растёт**, а `last_success` стоит — висит соединение, лечится
  `restart api`, а не заменой узлов.

## Другой тип подписки (не Reality)

Скрипт пропускает всё, что не `security=reality`. Если провайдер отдаёт
`ws`+`tls`, outbound пишется руками:

```json
"streamSettings": {
  "network": "ws",
  "security": "tls",
  "tlsSettings": { "serverName": "<SNI>", "fingerprint": "chrome" },
  "wsSettings": { "path": "<PATH>", "headers": { "Host": "<HOST>" } }
}
```

и `flow` у пользователя оставить пустым (`""`).
