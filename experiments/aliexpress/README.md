# AliExpress.ru — проб антибота (Camoufox)

Цель: понять, **можно ли вообще скрейпить aliexpress.ru с чистого датацентр-IP VPS**,
прежде чем городить полноценный скрейпер.

## Что уже выяснено (с dev-машины через vless)

- Страница товара `newDetail` — **CSR**: в HTML цены нет (`window.runParams = {}`),
  данные тянутся отдельным подписанным API (mtop `acs.aliexpress.com` / `aer-api`).
- Голый HTTP (curl) **и даже Chrome-JA3 через tls-client** (которым ходят ЯМаркет/Ozon)
  упираются в антибот **X5SEC**: вместо карточки приходит «punish»-страница
  (`_____tmd_____/punish`, `x5secdata`, `action:captcha`, `rgv587_flag:sm`).
  Тот же класс защиты, что FAB у Ozon.
- Значит HTML-парсер (как у ЯМаркета через JSON-LD) **не подойдёт**.

## Гипотеза этого проба

Антидетект-браузер **Camoufox** (патченый Firefox + правдоподобный fingerprint)
проходит X5SEC прозрачно (как настоящий браузер), и тогда цену можно снять из
DOM либо из перехваченного API-ответа. Тестируем **с датацентр-IP VPS, без прокси** —
ровно тот сценарий, который нужен в проде (vless дорогой/лишний, если IP и так пускают).

## Как запустить (на VPS, НЕ через vless)

```bash
# системные либы для Firefox + Xvfb (один раз, нужен sudo).
# ВНИМАНИЕ Ubuntu 24.04: пакеты с суффиксом t64 (libasound2 — виртуальный):
sudo apt-get update && sudo apt-get install -y \
  libgtk-3-0t64 libx11-xcb1 libasound2t64 libdbus-glib-1-2 libxtst6 libxt6t64 xvfb

# поставить и запустить проб:
bash experiments/aliexpress/run_probe.sh
```

Если Xvfb не поставлен — проб сам сфолбэчится на нативный headless (чуть менее
скрытно). Для полноценного virtual-режима хватает одного пакета: `sudo apt-get install -y xvfb`.

Без xvfb — нативный headless (чуть менее скрытно):

```bash
HEADLESS_MODE=true bash experiments/aliexpress/run_probe.sh
```

Другой товар:

```bash
ALI_ID=1005005863682926 bash experiments/aliexpress/run_probe.sh
```

## Что смотреть в выводе

Блок `ВЕРДИКТ`:
- `punish/captcha: нет ✅` + `цена на странице: НАЙДЕНА ✅` → **Camoufox проходит**,
  можно строить скрейпер (браузер-сайдкар, по образцу `OZON_BROWSER_URL`).
- `punish/captcha: ДА ❌` → даже браузер с датацентр-IP не пускают → нужен
  резидентский/RU-прокси (или vless), либо подход меняем.

Артефакты для разбора парсера (пришли их мне):
- `/tmp/aliprobe/page.html` — отрендеренный DOM,
- `/tmp/aliprobe/page.png` — скриншот (видно, punish это или карточка),
- `/tmp/aliprobe/resp_*.json` — перехваченные API-ответы с ценой (цель парсера).

## Если проб проходит — что дальше

Скрейпер AliExpress = **браузер-сайдкар** (Camoufox/Playwright-сервис) + Go-обёртка,
реализующая `scraper.MarketplaceScraper`, по образцу Ozon (`OZON_BROWSER_URL`).
`marketplace` в БД — `TEXT`, миграция не нужна; точки интеграции — как у Ozon
(`internal/telegram/bot.go` coming-soon, `cmd/scraper/main.go` registry).
