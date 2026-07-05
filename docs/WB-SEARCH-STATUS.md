# WB-поиск: wbaas режет горячие запросы 403 (диагноз + фикс)

Статус: **фикс реализован — браузерный сайдкар `wb-search-miner`.** Ветка
`fix/wb-search-403-proxy-fallback`. Ждёт деплой + прод-проверку.

## Симптом

`__internal/u-search` (v18) отдаёт **403 на ПОПУЛЯРНЫХ запросах** (`iphone 17`),
но **200 на редких** (`капибара`) — при валидном токене. Токены/майнер живы 5/5
на обоих пулах. Подтверждено на проде reseller-worker 2026-07-05.

## Диагноз: причина — ТРАНСПОРТ, не IP

Проверено в сессии 2026-07-05:

1. В проде **не задан ни один прокси-env** — token-miner минтит токены **direct
   с датацентр-IP** и в браузере (patchright Chromium) **проходит wbaas, ловит
   200 на `/u-search/`**.
2. Go-воркер ходит **с того же хоста = того же датацентр-IP** тем же cookie и
   получает **403 на горячих**, **200 на холодных**.

IP держится константой (майнер и воркер на одном IP). Браузер проходит,
`http.Client` — нет. Значит различие — **транспорт**: у браузера есть то, чего
нет у голого HTTP (TLS/JA3-фингерпринт и/или JS-вычисляемый динамический
заголовок/челлендж, который wbaas строже проверяет на коммерчески горячих
запросах; на редких проверка мягче и cookie достаточно).

Вывод: **резидентный прокси не решает** (меняет IP, но plain-HTTP остаётся
plain-HTTP). Прокси-подход отброшен (и прокси в проде мы вообще отменили).
Настоящий фикс — увести горячий запрос в браузер, как у Ozon с FAB.

## Фикс: браузер-как-транспорт (реализовано)

**Сайдкар `wb-search-miner/`** (по образцу `ozon-miner`, движок patchright
Chromium в Xvfb — тот же, что уже проходит wbaas в token-miner):
- держит прогретые дорожки: навигация на страницу поиска нейтрального запроса
  (проходит wbaas-стену) → healthy, когда in-page fetch к u-search даёт 200;
- `GET /search?query=&sort=&page=` → in-page fetch к тому же u-search из
  доверенного контекста, отдаёт **сырой JSON** формы `wbSearchResponse`, зеркаля
  статус (403 при стойкой стене);
- `/healthz`, `/metrics`; maintenance-цикл: перепрогрев нездоровых с backoff,
  keepalive живых. Прокси не нужен.

**Go `WildberriesSearchScraper`** (`WB_SEARCH_BROWSER_URL`):
- **direct-with-token first** (холодные запросы дёшевы, идут напрямую);
- на 403 direct → фолбэк в сайдкар (`fetchViaBrowser`), с **залипанием**
  `preferBrowser` на остальные страницы запроса (не тратим по 403 на страницу);
- парсинг общий (сайдкар отдаёт ту же форму JSON).

**Метрика** `pt_wb_search_fetch_total{transport,result}` (direct|browser ×
ok|forbidden|429|other|error): видно долю 403 direct и спасает ли браузер.

Тесты: `TestScrapeSearchFallbackToBrowser`, `TestScrapeSearchNoSidecarReturnsBlocked`.

## Деплой

Собрать сайдкар + перекатить воркеры (compose уже прописан, env с дефолтами):

```bash
COMPOSE="docker compose -f docker-compose.yml -f docker-compose.prod.yml"
$COMPOSE build wb-search-miner search-worker reseller-worker
$COMPOSE up -d wb-search-miner
$COMPOSE up -d search-worker reseller-worker
# перезагрузить prometheus-конфиг (добавлен скрейп wb-search-miner:8081)
$COMPOSE restart prometheus
```

Проверка:
```bash
$COMPOSE logs -f --tail=80 wb-search-miner   # ждём «дорожка 0 прогрета: u-search 200»
$COMPOSE exec -T wb-search-miner sh -c 'wget -qO- localhost:8081/healthz'
# горячий запрос через сайдкар:
$COMPOSE exec -T wb-search-miner sh -c 'wget -qO- "localhost:8081/search?query=iphone%2017&page=1" | head -c 300'
```

В Grafana/Prometheus: `pt_wb_search_fetch_total{transport="browser",result="ok"}`
растёт на горячих; `wb_search_miner_healthy_lanes == 1`.

## Открытые хвосты

- Одна дорожка (`WB_SEARCH_POOL_SIZE=1`) — если горячих запросов много, поднять
  пул (+ опц. `WB_LANE_<i>_PROXY`), как у ozon-miner.
- Алерт на `wb_search_miner_healthy_lanes == 0` (по образцу ozon-miner в
  `monitoring/prometheus/rules/alerts.yml`) — добавить после подтверждения на проде.
- Держать браузер горячим только под горячие запросы: direct покрывает холодную
  массу бесплатно, нагрузка на дорожку минимальна (как задумано у Ozon).
