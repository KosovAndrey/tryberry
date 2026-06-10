# Xray (VLESS) — основной egress в Telegram

На RU-хостинге РКН душит Telegram. Раньше весь egress шёл через WireGuard
(`wg-proxy` + `tinyproxy`), но WG — это UDP, и DPI его прицельно троттлит →
зависания long-poll на десятки секунд. VLESS идёт по TCP под видом обычного
HTTPS (особенно **VLESS+Reality** — маскируется под TLS к чужому сайту), и
почти не троттлится. Поэтому VLESS теперь основной выход, а WG — резерв.

## Как это устроено

Один контейнер `xray` поднимает локальный HTTP-прокси на `:8888`. У него два
плеча выхода (outbound):

- **`vless`** — основной не-RU exit (твоя подписка);
- **`wg`** — резерв через `http://wg-proxy:8888` (тот же старый WG-туннель).

`observatory` каждые 30с пробит оба плеча до `api.telegram.org`, а `balancer`
(`leastPing`) держит трафик на живом и быстром. Если VLESS-узел отвалится —
автоматически уходим на WG, без рестартов. `xray` **не зависит** от `wg-proxy`
при старте: если WG-плечо снято/сломано, VLESS работает сам.

Сервисы `api`/`bot-worker`/`notifier` ходят сюда как `HTTPS_PROXY=http://xray:8888`.

## Установка (на сервере)

```bash
cp xray/config.json.example xray/config.json   # config.json — секрет, в .gitignore
# заполнить плейсхолдеры из vless://-ссылки (таблица ниже), затем:
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d xray
# перезапустить потребителей прокси, чтобы перецепились на xray:
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d api bot-worker notifier
```

## Маппинг из vless://-ссылки

Ссылка подписки выглядит так:

```
vless://<UUID>@<SERVER_HOST>:<PORT>?type=tcp&security=reality&sni=<SNI>&pbk=<PUBLIC_KEY>&sid=<SHORT_ID>&flow=xtls-rprx-vision&fp=chrome#name
```

| Поле в ссылке            | Куда в `config.json` (outbound `vless`)                     |
|--------------------------|------------------------------------------------------------|
| `<UUID>` (до `@`)        | `settings.vnext[0].users[0].id`                            |
| `<SERVER_HOST>`          | `settings.vnext[0].address`                               |
| `<PORT>`                 | `settings.vnext[0].port`                                  |
| `flow=...`               | `settings.vnext[0].users[0].flow` (Reality: `xtls-rprx-vision`; WS/gRPC: `""`) |
| `sni=...`                | `streamSettings.realitySettings.serverName`              |
| `pbk=...`                | `streamSettings.realitySettings.publicKey`               |
| `sid=...`                | `streamSettings.realitySettings.shortId`                 |
| `fp=...`                 | `streamSettings.realitySettings.fingerprint`             |
| `type=tcp`               | `streamSettings.network`                                 |
| `security=reality`       | `streamSettings.security`                                |

> Если твоя подписка не Reality, а **WS+TLS** или **gRPC** (`type=ws`/`grpc`,
> `security=tls`) — пришли ссылку, поправлю `streamSettings`. Для WS обычно так:
>
> ```json
> "streamSettings": {
>   "network": "ws",
>   "security": "tls",
>   "tlsSettings": { "serverName": "<SNI>", "fingerprint": "chrome" },
>   "wsSettings": { "path": "<PATH>", "headers": { "Host": "<HOST>" } }
> }
> ```
> и `flow` у пользователя оставить пустым (`""`).

## Проверка

```bash
# Какой IP видит Telegram через xray (должен быть НЕ-RU):
docker compose exec xray sh -c "wget -qO- -e https_proxy=http://127.0.0.1:8888 https://api.ipify.org"
# Латентность через прокси бота:
curl -s -x http://<server>:.. # внутри: через http://xray:8888
# Логи выбора плеча / observatory:
docker compose logs -f xray
```

Если в логах `vless` помечается мёртвым, а трафик идёт через `wg` — проверь
поля Reality (`pbk`/`sid`/`sni` должны точно совпадать с сервером) и что
`<SERVER_HOST>:<PORT>` доступен с сервера.
