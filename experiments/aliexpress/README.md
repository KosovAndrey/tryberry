# AliExpress.ru — Phase-1 проб антибота (как у Ozon)

Цель: понять, **можно ли скрейпить aliexpress.ru с чистого датацентр-IP VPS**,
прежде чем строить полноценный сайдкар. Это в точности фаза 1 метода Ozon
(`ozon-miner/probe.py`): поднять реальный браузер, проверить гипотезу, потом уже
городить пул.

## Что уже выяснено

- Карточка товара `newDetail` — **CSR**: в HTML цены нет (`window.runParams = {}`),
  данные тянутся отдельным подписанным API (mtop `acs.aliexpress.com` / `aer-api`).
- Голый HTTP (curl) **и Chrome-JA3 через tls-client** (которым ходят ЯМаркет/Ozon)
  упираются в антибот **X5SEC**: вместо карточки — «punish»-страница
  (`_____tmd_____/punish`, `x5secdata`, `action:captcha`, `rgv587_flag:sm`). Тот же
  класс защиты, что FAB у Ozon → HTML-парсер как у ЯМаркета тут не годится.

## Гипотеза

Реальный браузер **camoufox** (headful в Xvfb) проходит X5SEC прозрачно, как у
Ozon с FAB. Тогда цену берём из отрендеренного DOM и/или перехваченного API-ответа.
Тестируем **с датацентр-IP VPS, без прокси** — нужный прод-сценарий.

## Как запустить (на VPS)

Проб гоняется **внутри образа ozon-miner** — там уже camoufox + Xvfb + все либы,
поэтому никаких ручных `apt`/`libgtk`/`xvfb` на хосте (об это спотыкался venv).

```bash
# на VPS, в репо:
git fetch origin && git checkout feat/aliexpress-camoufox

# образ camoufox должен быть собран (если ещё нет):
docker compose build ozon-miner

# запуск проба (без прокси, чистый датацентр-IP):
bash experiments/aliexpress/run.sh
```

Другой товар: `ALI_ID=1005005863682926 bash experiments/aliexpress/run.sh`.
Если без прокси не пускает — `ALI_PROXY=http://user:pass@host:port bash experiments/aliexpress/run.sh`.

## Что смотреть

Блок `ВЕРДИКТ` в выводе:
- `X5SEC punish: нет — пройден ✅` + `цена доступна: ДА ✅` → **camoufox проходит**,
  строим сайдкар `ali-miner` по образцу `ozon-miner` (browser-как-транспорт).
- `X5SEC punish: ДА ❌` → даже браузер с датацентр-IP не пускают → нужен
  резидентский/RU-прокси (или vless), либо меняем подход.

Артефакты в `experiments/aliexpress/out/` (пришли их мне):
- `page.png` — скриншот (сразу видно punish или карточку),
- `page.html` — отрендеренный DOM,
- `resp_*.json` — перехваченные API-ответы с ценой (цель парсера).

## Если проб проходит — план

Скрейпер AliExpress = **сайдкар `ali-miner`** (клон `ozon-miner`: aiohttp +
пул camoufox-дорожек, `GET /scrape?id=<id>`) + Go-обёртка `AliexpressScraper`,
реализующая `scraper.MarketplaceScraper` и ходящая в сайдкар (как `fetchViaBrowser`
у Ozon). `marketplace` в БД — `TEXT`, миграция не нужна; точки интеграции — как у
Ozon (`cmd/scraper/main.go` registry, `internal/telegram/bot.go` coming-soon).
