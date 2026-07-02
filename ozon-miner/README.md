# ozon-miner (фаза 1 — проба)

Цель майнера — то же, что у `token-miner` для WB, но для Ozon: раз в N часов
поднимать реальный Chromium (Patchright) **через тот же прокси, что у скрейпера**,
естественно проходить антибот **FAB**, забирать веб-куки сессии и класть в Redis,
откуда читает Go-скрейпер в **web-режиме** (`OZON_API_MODE=web`).

Это лечит корень падений 15-июня: FAB челленджил (`fab_chlg_`, 403) подделанную
mobile-связку `cookie + статичный x-o3-fp + рандомный gaid`, снятую на планшете
(чужая сеть). Реальный браузер через прод-прокси формирует **когерентный**
отпечаток на нужном egress — челлендж не выдаётся.

## Фаза 1: `probe.py` — проверить гипотезу до постройки пула

Отвечает на: (1) проходит ли **анонимный** браузер FAB через прод-прокси,
(2) видна ли цена **без логина**, (3) какой набор cookie выдаёт сессия.

Гоняется в уже собранном образе `pt_token_miner` (Patchright+браузеры+Xvfb уже там):

Проба **самодостаточна**: сама поднимает Xvfb и сама знает реальный товар по
умолчанию — команда тривиальная, без возни с дисплеем. Гоняется в готовом образе
`pt_token_miner` (Patchright+браузеры уже там), сборка нового образа не нужна.

Перед запуском впиши в `.env` **свежую cookie залогиненного аккаунта** (`OZON_COOKIE=`)
и мобильный прокси (`OZON_PROXY_URL=`) — это и есть метод друга.

```bash
cd ~/projects/tryberrybot
set -a; . ./.env; set +a                                  # подтянуть OZON_COOKIE/OZON_PROXY_URL
NET=$(docker network ls --format '{{.Name}}' | grep -m1 tryberry)

docker run --rm --network "$NET" \
  -v "$PWD/ozon-miner/probe.py:/app/probe.py" \
  -e OZON_COOKIE="$OZON_COOKIE" \
  -e OZON_MINER_PROXY_URL="$OZON_PROXY_URL" \
  --entrypoint python pt_token_miner /app/probe.py
```

`OZON_PROBE_URL` можно не задавать — дефолт это реальный товар (mixit, не 18+). Хочешь
другой — добавь `-e OZON_PROBE_URL="https://www.ozon.ru/product/...-<id>/"` с ПОЛНЫМ URL.

### Трактовка вердикта (exit code)

| Код | Вердикт | Следующий шаг |
|---|---|---|
| ✅ `0` | метод друга работает (200 + widgetStates + ₽) | строим/деплоим пул (`server.py`), Go → `OZON_API_MODE=browser` |
| ❌ `2` | FAB-инцидент `nmk` = жёсткий бан egress (датацентр/спалённый IP) | сменить мобильный IP ссылкой-ротацией, повторить |
| ⚠️ `3` | заход анонимный (нет `__Secure-access-token`) | подставить cookie залогиненного аккаунта |
| ❌ `4` | залогинен, но FAB-челлендж | свежая cookie из приложения / сильнее анти-детект; иначе fallback на платный API |

После зелёной пробы (код `0`) — фаза 2 (ниже).

## Фаза 2: `server.py` — браузер-как-транспорт, ПУЛ ДОРОЖЕК

Долгоживущий сервис (`pt_ozon_miner`). На каждую **дорожку** (lane) — отдельный
залогиненный Chromium через свой мобильный прокси и свою аккаунт-cookie. Живая
сессия проходит FAB и сама рефрешит access-token. Цену достаём **методом друга**:
in-page fetch к `entrypoint-api` изнутри доверенного контекста (как в `probe.py`).

Go-скрейпер в browser-режиме (`OZON_API_MODE=browser`, `OZON_BROWSER_URL=
http://ozon-miner:8080`) зовёт `GET /scrape?id=<product_id>` → сервис маршрутизирует
на дорожку (аффинити по id) → отдаёт сырой `widgetStates`, зеркаля upstream-статус
(в т.ч. 403 на FAB). Go разбирает тем же `parseOzonWidgets`. Есть `GET /healthz`.

### Масштабирование

Узкое место — **не браузер, а связка {мобильный IP + аккаунт}**: FAB бьёт по
репутации IP и частоте с него. Растём, добавляя дорожки (по IP+аккаунту):

```env
OZON_POOL_SIZE=3
OZON_LANE_0_PROXY=http://user:pass@host0:port   # дорожка 0 фолбэчит на OZON_PROXY_URL
OZON_LANE_0_COOKIE=...                           # ... и OZON_COOKIE
OZON_LANE_1_PROXY=http://user:pass@host1:port   OZON_LANE_1_COOKIE=...
OZON_LANE_2_PROXY=http://user:pass@host2:port   OZON_LANE_2_COOKIE=...
OZON_LANE_MIN_INTERVAL_MS=1500                   # человекоподобный интервал/дорожку
```

Одна дорожка держит ≈ низкие сотни товаров при 20-мин кадансе (точную ёмкость даёт
спайк «сколько req/мин до челленджа»). Стоимость растёт линейно по дорожкам
(прокси+аккаунт), внутри дорожки — flat. Дешевле новых дорожек — растягивать каданс
низкоприоритетных товаров (`OZON_MIN_INTERVAL_MINUTES`).

## Фаза 3: анонимный / гибридный режим (2026-07-02)

`probe.py` c пустым `OZON_COOKIE` доказал: **camoufox проходит FAB БЕЗ логина**
(status=200, widgetStates+цена). Аккаунт-cookie был легаси-грузом из до-camoufox
эпохи — обычным товарам он не нужен. Нужен **только под 18+** (нож/алкоголь/табак):
анонимная сессия упирается в возрастной гейт (ввод даты рождения).

**Гибрид (реализован):**
- Дорожка **без cookie** = анонимная (штатная). Обычный поток идёт через неё —
  аккаунт не светится → нечего банить, масштаб только по IP.
- Дорожка **с cookie** (залогинена, 18+ подтверждён) = `authed`, резерв под 18+.
- Go на анонимный `ErrAgeRestricted` ретраит `GET /scrape?id=<id>&authed=1` →
  сайдкар берёт `authed`-дорожку. Нет authed-дорожки → Go отдаёт `ErrAgeRestricted`
  (как раньше).

Пример конфига (2 анонимные дорожки + 1 authed под 18+):
```env
OZON_POOL_SIZE=3
OZON_LANE_0_PROXY=http://user:pass@host0:port     # аноним (без _COOKIE)
OZON_LANE_1_PROXY=http://user:pass@host1:port     # аноним
OZON_LANE_2_PROXY=http://user:pass@host2:port
OZON_LANE_2_COOKIE=__Secure-access-token=...;...  # authed → 18+
```
Чисто анонимный прод (без 18+): просто не задавать ни одной `_COOKIE`.

Метрики: `ozon_miner_authed_lanes` (0 = 18+ недоступны), `ozon_miner_lane_healthy{authed="0|1"}`.

### `probe_load.py` — серийный тест «жизнь без мобильного прокси»

Открытие: одиночный probe прошёл FAB анонимно **даже с датацентр-IP** (без прокси).
Но мобильный прокси ценен под НАГРУЗКОЙ. Скрипт эмулирует одну дорожку под боевым
кадансом и считает `blocked_rate`. ⚠️ Может подпалить боевой egress-IP — гоняй с
изолированного IP или будь готов переждать.

`--network` НЕ нужен: probe ходит напрямую в интернет (ozon.ru + прокси), а не во
внутреннюю docker-сеть. Монтируем ПАПКУ `ozon-miner` в `/probe` и запускаем оттуда
(если скрипта нет в собранном образе — bind-mount его подтащит; при отсутствии
файла-источника docker создаёт пустую папку → "can't find '__main__'").

```bash
cd ~/projects/tryberrybot
set -a; . ./.env; set +a

# БЕЗ прокси (датацентр-direct) — проверяем, можно ли жить без мобильного:
docker run --rm \
  -v "$PWD/ozon-miner:/probe" \
  -e OZON_PROXY_URL="" \
  -e OZON_LOAD_IDS="1889984997,<id2>,<id3>" \
  -e OZON_LOAD_N=80 -e OZON_LOAD_INTERVAL_S=20 \
  --entrypoint python tryberrybot-ozon-miner /probe/probe_load.py
```
Затем прогнать второй раз с `OZON_PROXY_URL="$OZON_PROXY_URL"` и сравнить
`blocked_rate`. Задай **реальные id из трек-листа** в `OZON_LOAD_IDS` (дефолт — один
товар, мало показателен). Образ — `tryberrybot-ozon-miner` (там camoufox), не token-miner.

### Статус скелета (TODO до прода)

- [ ] прогнать `probe.py` с залогиненной cookie → подтвердить, что метод друга даёт 200;
- [ ] уточнить маркер «сессия жива» в `Lane.warm()` (сейчас best-effort по `_FAB_RE`);
- [ ] замерить TTL FAB-стейта → перепрогрев/ротация cookie дорожки по таймеру;
- [ ] метрики/алерты здоровья пула (healthy lanes) — по образцу `wb_tokens`.
