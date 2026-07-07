# ali-miner — браузер-как-транспорт для поисковой выдачи aliexpress.ru

Сайдкар по образцу `wb-search-miner`/`ozon-miner`. Держит пул прогретых дорожек
(headful Chromium/patchright в Xvfb), проходит X5SEC живой сессией и отдаёт
выдачу `/aer-webapi/v1/search` перехватом нативного XHR фронта.

## Зачем

Голый direct-POST к `/aer-webapi/v1/search` с датацентр-IP X5SEC режет
(punish/слайдер-капча), хотя карточный `productData` с той же прогретой
aer-cookie проходит — выдачу антибот проверяет строже. Go-скрейпер
(`internal/scraper/aliexpress_search.go`) сначала пробует direct, а на
blocked/пустой ответ уходит сюда (`ALI_BROWSER_URL`), как WB на 403 в
`wb-search-miner`.

## Контракт

- `GET /search?text=<запрос>&page=<n>` → сырой JSON `/aer-webapi/v1/search`
  (та же форма, что direct → `parseAliexpressSearch` общий), статус зеркалит
  upstream (403/пусто при стойкой стене). Заголовок `X-Ali-Lane`.
- `GET /healthz` → `{healthy,total,lanes}`, 200 если есть живая дорожка иначе 503.
- `GET /metrics` → `ali_miner_healthy_lanes` / `ali_miner_lane_healthy`.

## Ключевые env

| Env | Дефолт | Смысл |
|-----|--------|-------|
| `ALI_MINER_PORT` | 8082 | порт HTTP |
| `ALI_SEARCH_POOL_SIZE` | 1 | число дорожек (браузеров) |
| `ALI_WARM_QUERY` | телефон | нейтральный запрос прогрева |
| `ALI_SEARCH_PROXY_URL` / `ALI_LANE_<i>_PROXY` | — | опц. RU-прокси (обычно НЕ нужен — direct проходит) |
| `ALI_WARM_RELAUNCH_AFTER` | 3 | пересоздать браузер после N неудач прогрева |
| `HEADLESS` | false | headful обязателен (иначе X5SEC палит) |

## Как проверить локально (нужен рабочий Docker; в WSL нет)

```sh
docker build -t ali-miner ./ali-miner
docker run --rm -p 8082:8082 -e HEADLESS=false ali-miner
# в другом терминале, после прогрева (лог «дорожка 0 прогрета»):
curl 'http://localhost:8082/search?text=телефон&page=1' | head -c 400
curl http://localhost:8082/healthz
```

## Прогрев считается успешным

когда СОБСТВЕННЫЙ search-XHR страницы `/wholesale?SearchText=…` вернул 200, тело
без punish-маркеров и с `"snippetContainer"` (есть товары). Ручной in-page fetch
к search НЕ делаем — фронт кладёт в запрос волатильные `bx-*`-заголовки/подпись,
которые руками не воспроизвести (тот же урок, что у WB u-search).
