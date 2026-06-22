# ali-miner — браузер-как-транспорт для aliexpress.ru

Отдельный сервис по образцу `ozon-miner`, но **свой образ и БЕЗ прокси**: Ali
ходит напрямую с датацентр-IP VPS (Ozon — через прокси, поэтому мешать в один
образ нельзя). Camoufox (анти-детект Firefox) headful в Xvfb.

## Зачем

- Карточка aliexpress.ru — **CSR**: цены в HTML нет, данные грузит отдельный
  подписанный API (mtop `acs.aliexpress.com` / aer-api).
- Голый HTTP и даже Chrome-JA3 через tls-client упираются в антибот **X5SEC**
  («punish»-страница с `x5secdata`/`_____tmd_____`) — тот же класс, что FAB у Ozon.
- Решение — реальный браузер (camoufox), который проходит X5SEC живой сессией.
  Дальше цену берём из DOM / перехваченного API.

## Phase 1 — проб (проверить гипотезу до постройки пула)

Как у `ozon-miner` (сперва `probe.py`, потом пул). Образ несёт `probe.py`.

```bash
# на VPS, в репо:
git fetch origin && git checkout feat/aliexpress-camoufox && git pull

# собрать образ и прогнать проб (без прокси, датацентр-IP):
bash ali-miner/run.sh
```

Всё нужное (camoufox, Xvfb, GTK/X11-либы) — внутри образа (`playwright
install-deps firefox`), на хосте ничего ставить не надо.

### Что смотреть

Блок `ВЕРДИКТ`:
- `X5SEC punish: нет — пройден ✅` + `цена доступна: ДА ✅` → строим Phase-2.
- `X5SEC punish: ДА ❌` → датацентр-IP не пускают даже браузер → нужен
  резидентский/RU-прокси (`ALI_PROXY=...`), либо меняем подход.

Артефакты в `ali-miner/out/` (пришли их): `page.png`, `page.html`, `resp_*.json`.

## Phase 2 — пул дорожек (после успешного проба)

Добавится `server.py` (aiohttp + пул camoufox-дорожек, `GET /scrape?id=<id>`),
`CMD` в Dockerfile сменится на `server.py`, сервис `ali-miner` пропишется в
`docker-compose.yml` (как `ozon-miner`, но без proxy/lane-cookie). Go-обёртка
`AliexpressScraper` (`internal/scraper`) будет ходить в сайдкар по образцу
`fetchViaBrowser` у Ozon. `marketplace` в БД — `TEXT`, миграция не нужна.
