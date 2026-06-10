# Xray (VLESS) — основной egress в Telegram

На RU-хостинге РКН душит Telegram. Раньше весь egress шёл через WireGuard
(`wg-proxy` + `tinyproxy`), но WG — это UDP, и DPI его прицельно троттлит →
зависания long-poll на десятки секунд. VLESS идёт по TCP под видом обычного
HTTPS (особенно **VLESS+Reality** — маскируется под TLS к чужому сайту), и
почти не троттлится. Поэтому VLESS теперь основной выход, а WG — резерв.

## Как это устроено

Один контейнер `xray` поднимает локальный HTTP-прокси на `:8888`. Плечи выхода
(outbound):

- **`vless-ger1` … `vless-ger6`** — 6 немецких узлов подписки un1.pro;
- **`wg`** — резерв через `http://wg-proxy:8888` (старый WG-туннель).

`observatory` каждые 30с пробит все плечи до `api.telegram.org`, `balancer`
(`leastPing`) держит трафик на **живом и самом быстром** узле. Узел отвалился —
автоматически уходим на следующий, без рестартов. Если умрут все 6 VLESS —
остаётся `wg`. Селектор балансировщика матчит теги по префиксу, поэтому
`["vless", "wg"]` охватывает все шесть `vless-*` сразу.

`xray` **не зависит** от `wg-proxy` при старте: если WG-плечо снято/сломано,
VLESS работает сам. Сервисы `api`/`bot-worker`/`notifier` ходят сюда как
`HTTPS_PROXY=http://xray:8888`.

## Почему 6 серверов вручную, а не «подписка»

Xray-core **не умеет подписочные ссылки** (`subs.un1.pro/...`) — их разворачивают
клиенты (v2rayN/Nekobox), ядро ест только статический `config.json`. Поэтому 6
узлов прописаны как 6 outbound. Это надёжнее динамики: всё под контролем и в git,
а хостнеймы `gerN.un1.pro` стабильны. Сменятся узлы — обновить `config.json`.

## Установка (на сервере)

```bash
cp xray/config.json.example xray/config.json   # config.json — секрет, в .gitignore
```

Вытащить полные параметры из подписки (UUID, pbk, sid, sni — одинаковые у всех 6):

```bash
curl -s "https://subs.un1.pro/<ТВОЙ_КОД>" | base64 -d
# выведет 6 строк vless://<UUID>@gerN.un1.pro:443?...&pbk=...&sid=...&sni=...
```

В `xray/config.json` заполнить **4 общих** плейсхолдера (значения одинаковы во
всех 6 блоках — удобно `sed`'ом или заменой в редакторе):

| Плейсхолдер              | Откуда в ссылке `vless://`                | Поле в конфиге                                  |
|--------------------------|-------------------------------------------|-------------------------------------------------|
| `<UUID>`                 | часть до `@`                              | `...users[0].id`                                |
| `<SNI>`                  | `sni=...`                                 | `realitySettings.serverName`                    |
| `<REALITY_PUBLIC_KEY>`   | `pbk=...`                                 | `realitySettings.publicKey`                     |
| `<SHORT_ID>`             | `sid=...` (если в ссылке нет — оставь `""`)| `realitySettings.shortId`                       |

Адреса `gerN.un1.pro` и `flow=xtls-rprx-vision` уже проставлены. Если в подписке
другие хостнеймы/порт — поправь `address`/`port` в блоках.

Поднять и перецепить потребителей прокси:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d xray
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d api bot-worker notifier
```

## Проверка

```bash
# Какой IP видит Telegram через xray (должен быть НЕ-RU, немецкий):
docker compose exec xray sh -c "wget -qO- -e https_proxy=http://127.0.0.1:8888 https://api.ipify.org"
# Логи выбора плеча / observatory:
docker compose logs -f xray
```

Если все `vless-*` помечаются мёртвыми, а трафик идёт через `wg` — проверь, что
`pbk`/`sid`/`sni` точно совпали с подпиской (Reality к ним чувствителен) и что
`gerN.un1.pro:443` доступны с сервера.

## Другой тип подписки (не Reality)

Шаблон собран под **VLESS+Reality / TCP** (`security=reality&type=tcp`). Если в
ссылке `type=ws`/`grpc` и `security=tls` — поменяй `streamSettings`, напр. для WS:

```json
"streamSettings": {
  "network": "ws",
  "security": "tls",
  "tlsSettings": { "serverName": "<SNI>", "fingerprint": "chrome" },
  "wsSettings": { "path": "<PATH>", "headers": { "Host": "<HOST>" } }
}
```

и `flow` у пользователя оставить пустым (`""`).
