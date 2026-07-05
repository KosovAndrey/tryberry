# wb-search-miner

Браузер-как-транспорт для WB-поиска. Долгоживущий сайдкар: держит прогретые
дорожки (headful Chromium через Xvfb, patchright) и отдаёт выдачу u-search
in-page fetch'ем из доверенного контекста.

## Зачем

Голый direct к `__internal/u-search` с датацентр-IP wbaas режет **403 на
популярных запросах** (`iphone 17`), хотя cookie-токен валиден (`капибара` идёт
200). Причина — **транспорт**, не IP: token-miner с того же IP в браузере ловит
200 на u-search. Поэтому горячие запросы уводим в браузер (как ozon-miner при
FAB). Прокси не нужен.

Диагноз и общий план — `docs/WB-SEARCH-STATUS.md`.

## API

- `GET /search?query=<q>&sort=<popular|pricedown|…>&page=<n>` → сырой JSON
  u-search (форма `wbSearchResponse`), статус зеркалит upstream (403 при стене).
- `GET /healthz` → `{healthy, total, lanes}`; 200 если жива хоть одна дорожка.
- `GET /metrics` → `wb_search_miner_healthy_lanes` и пр. (Prometheus text).

Зовёт его Go `WildberriesSearchScraper` как 403-фолбэк (`WB_SEARCH_BROWSER_URL`):
direct-first (холодные запросы дёшевы), на 403 — в сайдкар, с залипанием на
остальные страницы запроса.

## Ключевой конфиг (env)

| env | дефолт | что |
|-----|--------|-----|
| `WB_SEARCH_MINER_PORT` | `8081` | порт сервиса |
| `WB_SEARCH_POOL_SIZE` | `1` | число дорожек |
| `WB_WARM_QUERY` | `телефон` | запрос прогрева (проходит стену) |
| `WB_SEARCH_PROXY_URL` / `WB_LANE_<i>_PROXY` | — | опц. прокси (обычно НЕ нужен) |
| `WB_WARM_KEEPALIVE_MINUTES` | `30` | периодический re-warm живой дорожки |
| `HEADLESS` | `false` | headful в Xvfb (обязателен для обхода) |

## Прогрев

Навигация на страницу поиска `WB_WARM_QUERY` (проходим wbaas-стену) → ждём, что
in-page fetch к u-search отдаёт валидный 200 → дорожка healthy. Нездоровые
перепрогреваются в maintenance-цикле с экспоненциальным backoff (не долбим стену).
