# web/ — статика сайта tryberry.ru (webroot)

**Это корень статики сайта** — nginx монтирует `./web` → `/usr/share/nginx/html:ro`
(`docker-compose.prod.yml`). Раньше лежало в `nginx/html/`, перенесено сюда как
отдельная папка под сайт. Источник правды — здесь, правки через git/PR.

## Что внутри
- `index.html` — лендинг: hero, фичи, pinned-showcase стратегий, тарифы,
  оплата/возврат, FAQ, контакты, модалки `Политика`, `Согласие на обработку ПД` и `Оферта`
  (deep-link `#privacy` / `#consent` / `#offer`; `openLegal` закрывает соседние модалки).
- `spasibo/index.html` — страница «спасибо за покупку» (return-URL Робокассы).
- `fonts/` — **self-hosted** WOFF2 (latin+cyrillic, 22 файла) + `fonts.css`. Используют
  все страницы сайта И `/p/` от `cmd/api` (общий `/fonts/fonts.css`).
- `assets/` (chart.css/js), `vendor/` (uPlot) — версионируются `?v=<git-hash>`, их
  потребляет `cmd/api/templates/product.html`.
- `logo.png` — 256×256, отдаётся как `/logo.png`.

## Позиционирование (после ребрендинга hero)
Главное сообщение — **«отличаем настоящую скидку от накрученной»**, а не просто
«следим за ценой». Это наш уникальный актив (история цены + честный вердикт,
как на страницах `/p/<id>`, которые отдаёт `cmd/api`). Держим этот тон во всех правках.

## Дизайн-система (CSS-токены в `:root`)
- Фон тёмный «berry/plum»: `--ink #170711`, `--plum-deep #260a1d`; светлые секции `--cream #f9f3ef`.
- Акцент: `--berry #e8336c` / `--berry-bright #ff5d8f` / `--berry-deep #a01651`.
- Вердикт честности: `--good #3ad29f` (настоящая скидка), `--warn #ffb23e` (накрутка).
- Шрифты: `--font-display` Unbounded (заголовки), `--font-body` Onest (текст).
- Приёмы: зерно (`body::after`), glow-радиалы в hero, `[data-reveal]` появление по скроллу,
  pinned scroll-showcase (`.pin-*`), `@media(prefers-reduced-motion)` уважается.

## Деплой
nginx отдаёт `web/` с корня (см. `../nginx/conf.d/tryberry.conf`): `/assets/`, `/vendor/`,
`/fonts/` — с `Cache-Control: immutable` на год. А `/robots.txt`, `/sitemap.xml`,
страницы `/p/<id>` и `/api/*` проксируются на Go-сервис `cmd/api` (`webpages.go`) —
их HTML здесь НЕ лежит, но они тянут наши `/fonts/` и `/assets/`.

## Шрифты (self-hosted, готово)
`fonts/fonts.css` объявляет `@font-face` Unbounded (300–800) и Onest (300–700),
сабсеты latin+cyrillic. Символ ₽ (U+20BD) не входит ни в один сабсет Google —
рендерится системным фолбэком (так было и на CDN, не регресс). Менять веса/семейства —
перегенерить `fonts.css` и woff2 из Google css2 (см. историю коммита). Критичные для
первого экрана начертания идут `<link rel=preload>` в `<head>` страниц.

## JSON-LD (готово)
В `<head>` лендинга два блока `application/ld+json`: `Organization` и `FAQPage`
(8 вопросов, текст 1:1 с видимым FAQ — при правке FAQ обновлять и схему).

## Варианты дизайна (`variants/`)
`web/variants/` — песочница редизайна для выбора направления. Хаб сравнения:
`/variants/` (открыть `tryberry.ru/variants/` на деплое фич-ветки). Варианты:
`ledger` (чек/моно), `bold` (брутализм/стикер), `quiet` (премиум-тёмный),
`editorial` (журнал-расследование). Все — самодостаточный `index.html`, шрифты с
Google CDN (это прототипы), `<meta robots=noindex>`. Контент реальный, без выдуманных
цифр. Когда выбран финал — переносим в продакшн-вёрстку, self-host шрифтов, JSON-LD,
а `variants/` удаляем.

## Открытые улучшения (по приоритету)
1. **Соц-доказательства** в hero: число пользователей / пойманных накруток + отзывы-скриншоты.
   ⚠️ Нужны РЕАЛЬНЫЕ цифры — не выдумывать.
2. `llms.txt` (опционально) — для GEO; не критично (Google в 2026 объявил необязательным).
