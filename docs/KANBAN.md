# 📋 Канбан tryberrybot

**Основная доска — в Notion** (страница «🛠 tryberrybot — задачи», база «Канбан tryberrybot»;
есть Board + Calendar view + iOS-виджет). Этот файл — оффлайн-зеркало в git.

Правила: двигай карточку между секциями (`## Backlog` → `## In Progress` → `## Done`),
помечай `[x]` при готовности, в скобках — кто/контекст. Приоритет: 🔴 важно / 🟡 желательно /
🟢 мелочь.

Подробности по Ozon — `docs/OZON-STATUS.md`. Дата последнего апдейта: **2026-06-15**.

---

## 🟦 Backlog (надо сделать)

- [ ] 🔴 **Замерить срок жизни Ozon access-token** — дождаться алерта `ScrapeAuthExpired`,
      посчитать время от лога `ozon scraper configured` до алерта. Идёт фоном, бесплатно.
- [ ] 🔴 **Авто-рефреш Ozon-токена** (после замера) — refresh-флоу по `__Secure-refresh-token`,
      интервал ≈80% измеренного срока. Эталон: `Churkashh/ozon-pinneaples`.
- [ ] 🔴 **Отдельный прод-аккаунт Ozon** — не личный Андрея; личную сессию разлогинить.
- [ ] 🟢 Подстроить маркеры 18+/login по реальному ответу Ozon (сейчас эвристика).
- [ ] 🟢 Стабилизировать выбор заголовка `navTitle` (иногда отдаёт вторичный заголовок).
- [ ] 🟢 Оптимизировать латентность Ozon (мобильный прокси + egress-IP-чек на блоках).

## 🟨 In Progress / на тебе (деплой)

- [ ] **Деплой консолидации прокси на VPS**: в `.env` — `RESELLER_TOKEN_PROXY_URL`=мобильный,
      убрать `TELEGRAM_PROXY_URL` и старый lteboost; затем
      `up -d --force-recreate token-miner token-miner-reseller notifier`.

## 🟩 Done (15.06.2026, всё в `main`, merge `b33642f`)

- [x] Ozon end-to-end (путь B: аккаунт-cookie + composer-api + okhttp + мобильный прокси).
- [x] Парсер Ozon: цена `price.price[]` по `textStyle`, имя `navTitle`, фото `multimedia`.
- [x] Пол интервала Ozon (`OZON_MIN_INTERVAL_MINUTES`).
- [x] 18+ (`ErrAgeRestricted`) + сообщения в TG и VK.
- [x] Детект протухания токена (`ErrAuthExpired`) + критикал-алерт `ScrapeAuthExpired`.
- [x] Мониторинг: статусы scrape `proxy`/`auth`/`disabled` + алерты proxy/auth/disabled.
- [x] Выпилен `ozon-miner` + ETC-плумбинг; диагностические дампы подчищены.
- [x] `token-miner`: прокси в `.env` (был захардкожен секрет) + общий лок `MINE_GLOBAL_LOCK`.
- [x] VK: картинка товара в уведомлениях о снижении цены.
- [x] Постоянный мобильный прокси (1 мес), всё на одном IP.
- [x] Merge `feat/ozon-scraper` → `main`.
