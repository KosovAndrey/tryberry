# Этап 2 — пул WB-токенов + ротация + алерт + дашборд

> ⚠️ **ИСТОРИЧЕСКОЕ** — разовая инструкция одного этапа. Общий прод-деплой —
> `README-deploy.md`, карта доков — `docs/README.md`.

Архив кладётся в корень репо (`~/projects/tryberrybot`) и перезаписывает файлы.
Полные файлы (правки уже внутри): cmd/scraper/main.go, internal/scraper/*.go,
token-miner/miner.py, monitoring/* . Менять руками ничего не нужно, кроме
docker-compose (env/command — см. ниже), т.к. compose правился на сервере вживую.

## Правки в docker-compose.yml (вручную, т.к. файл расходится с репо)

token-miner:
    init: true                              # реап зомби (headful chromium плодит детей)
    environment:
      WB_TOKEN_POOL_SIZE: "5"

scraper:
    environment:
      WB_TOKEN_POOL_SIZE: "5"               # чтобы число попыток покрывало пул

redis-exporter: в command добавить два ключа к --check-single-keys:
    --check-single-keys=wb:search:token:mined_at,wb:search:token:exp,wb:search:pool:healthy,wb:search:pool:oldest_mined_at

## Стадия A — майнер-пул + метрики + алерт + дашборд (скрейпер не трогаем)

docker compose -f docker-compose.yml -f docker-compose.prod.yml build token-miner
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d token-miner
docker compose -f docker-compose.yml -f docker-compose.prod.yml rm -sf redis-exporter
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d redis-exporter
docker exec pt_prometheus kill -HUP 1
docker compose -f docker-compose.yml -f docker-compose.prod.yml restart grafana

# проверка
docker logs -f pt_token_miner               # "цикл готов: живых токенов 5/5"
docker exec pt_redis redis-cli MGET wb:search:pool:healthy wb:search:pool:oldest_mined_at

Скрейпер всё это время живёт на legacy-зеркале (майнер его пишет) — без простоя.

## Стадия B — Go: round-robin + mark-broken (через build-gate)

# build-gate в golang-контейнере: go build ./... && go test ./...
docker compose -f docker-compose.yml -f docker-compose.prod.yml build scraper
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d scraper
docker logs -f pt_scraper                    # search tick + query scraped без шквала 429

# тест ротации: пометить слот битым — майнер дольёт, скрейпер крутит остальные
docker exec pt_redis redis-cli HSET wb:search:pool:0 status broken
