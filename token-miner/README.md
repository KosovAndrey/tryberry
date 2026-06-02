# WB token miner (этап 1 — один токен)

Сайдкар-контейнер `pt_token_miner`. Раз в ~12 ч поднимает headless Chromium
(Patchright — стелс-форк Playwright), грузит страницу выдачи WB, дожидается
**200 на `/u-search/`**, забирает cookie `x_wbaas_token` + UA и пишет в Redis в
те же ключи, что читает Go-скрейпер. Заменяет ручной `scripts/wb-token-update.sh`.

Записывает в Redis: `wb:search:cookie`, `wb:search:ua`, `wb:search:token`
(+ `wb:search:token:mined_at`, `wb:search:token:exp` — для наблюдаемости).
При неудачной попытке **старый рабочий токен не перетирается**.

## Переменные окружения

| Переменная | Деф. | Назначение |
|---|---|---|
| `REDIS_URL` | `redis://redis:6379` | подключение к Redis |
| `WB_SEARCH_QUERY` | `телефон` | запрос для страницы выдачи (что угодно популярное) |
| `MINE_INTERVAL_HOURS` | `12` | пауза между успешными майнингами |
| `MINE_RETRY_MINUTES` | `10` | пауза перед повтором после неудачи |
| `MINE_TIMEOUT_SECONDS` | `90` | общий бюджет одной попытки |
| `MINE_MAX_RELOADS` | `4` | сколько раз перезагрузить страницу, добиваясь 200 |
| `MINE_ONCE` | `false` | один прогон и выход (для теста/cron) |
| `HEADLESS` | `true` | headless-режим |
| `MINER_BLOCK_RESOURCES` | `true` | резать картинки/шрифты/медиа (быстрее, легче трафик) |
| `MINER_BROWSER_CHANNEL` | _(пусто)_ | `chrome` → реальный Chrome вместо chromium (см. ниже) |
| `WB_TOKEN_PROXY_URL` | _(пусто)_ | `http://user:pass@host:port`, опционально |
| `WB_USER_AGENT` | _(пусто)_ | переопределить UA; пусто → реальный UA браузера |
| `MINER_LOCALE` / `MINER_TIMEZONE` | `ru-RU` / `Europe/Moscow` | локаль и таймзона контекста |
| `LOG_LEVEL` | `INFO` | `DEBUG`/`INFO`/`WARNING` |

## Локальный тест (один прогон)

```bash
cd token-miner
docker build -t pt_token_miner .
# headless, один прогон, пишет в Redis из docker-сети проекта:
docker run --rm --network tryberrybot_default \
  -e REDIS_URL=redis://redis:6379 -e MINE_ONCE=true -e LOG_LEVEL=DEBUG \
  pt_token_miner
# проверить результат:
docker exec pt_redis redis-cli GET wb:search:token | head -c 40; echo
docker exec pt_redis redis-cli GET wb:search:token:mined_at
```
(имя сети уточни: `docker network ls | grep tryberry`.)

## Подключение в compose

### `docker-compose.yml` — добавить сервис в блок `services:`

```yaml
  token-miner:
    build:
      context: ./token-miner
      dockerfile: Dockerfile
    container_name: pt_token_miner
    environment:
      REDIS_URL: "redis://redis:6379"
      WB_SEARCH_QUERY: "телефон"
      MINE_INTERVAL_HOURS: "12"
      MINE_RETRY_MINUTES: "10"
      MINE_MAX_RELOADS: "4"
      MINE_TIMEOUT_SECONDS: "90"
      HEADLESS: "true"
      LOG_LEVEL: "INFO"
      # MINER_BROWSER_CHANNEL: "chrome"            # если chromium начнёт палиться
      # WB_TOKEN_PROXY_URL: "${WB_TOKEN_PROXY_URL:-}"
    shm_size: "1gb"                                 # Chromium любит /dev/shm
    depends_on:
      redis:
        condition: service_healthy
    labels:
      logging: "promtail"
      service: "token-miner"
```

### `docker-compose.prod.yml` — добавить рестарт-политику

```yaml
  token-miner:
    restart: unless-stopped
    # при необходимости прокси на проде:
    # environment:
    #   WB_TOKEN_PROXY_URL: "${WB_TOKEN_PROXY_URL}"
```

### Поднять

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml build token-miner
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d token-miner
docker logs -f pt_token_miner   # ждём "ТОКЕН ОБНОВЛЁН: …"
```

## Если не берёт токен (всегда 429/нет 200)

1. Посмотреть в логах статусы (`статусы: [...]`).
2. Запустить headful на машине с дисплеем (`HEADLESS=false`) и глазами посмотреть,
   что показывает страница (челлендж/капча/пусто).
3. Переключить на реальный Chrome: в Dockerfile заменить
   `patchright install --with-deps chromium` на `... chrome`, в env поставить
   `MINER_BROWSER_CHANNEL=chrome`. Это самый сильный «патч» Patchright.
4. Поднять `MINE_MAX_RELOADS`/`MINE_TIMEOUT_SECONDS`.
5. Включить прокси `WB_TOKEN_PROXY_URL` (RU).
6. Крайний случай — движок Camoufox (Firefox, сильнее антидетект) как отдельный
   вариант; для этапа 1 не требуется.

---

## ⚠️ Хвост из HANDOFF §4 — интервалы 10/20 (проверить на сервере)

В бандле этих переменных нет — они правились на сервере вживую, могли попасть не
в тот блок `environment`. Должно быть так:

```yaml
# docker-compose.yml
services:
  api:
    environment:
      SCRAPE_INTERVAL_MINUTES: "10"          # ← товарный шедулер у api
  scraper:
    environment:
      SEARCH_SCRAPE_INTERVAL_MINUTES: "20"   # ← поиск-цикл у scraper
```

Применить и проверить:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d api scraper
docker exec pt_api     printenv | grep -i INTERVAL   # ждём SCRAPE_INTERVAL_MINUTES=10
docker exec pt_scraper printenv | grep -i INTERVAL   # ждём SEARCH_SCRAPE_INTERVAL_MINUTES=20
docker logs pt_scraper | grep -i interval            # ждём "interval":"20m0s"
```

Если у scraper переменная пустая — значит строка `SEARCH_SCRAPE_INTERVAL_MINUTES`
лежит в `environment` сервиса `api` (или с неправильным отступом). Перенести её в
`scraper: → environment:`, у `api` оставить только `SCRAPE_INTERVAL_MINUTES`, затем
повторить `up -d api scraper`.
