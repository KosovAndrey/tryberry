# Индекс документации tryberrybot

Оглавление всех доков со статусом. **Правило: у каждой темы один канон.** Если
два дока противоречат — прав тот, что помечен 📌 КАНОН. Обновлено 2026-07-06.

## Источники правды (не в docs/)

- **Задачи/планы** → **Google Tasks** (список «tryberrybot», `scripts/gtasks.py`).
  Это единственный источник по задачам. Старые доски-журналы ниже (📦) — историчны.
- **Код** → сам код (`domain.Plans`, `cmd/scheduler`, миграции) важнее любого дока.
- **Секреты/конфиг прода** → `.env` на сервере (в git нет).

## 📌 Канон / справочники (актуально, поддерживать в первую очередь)

| Док | О чём |
|---|---|
| `SCRAPE-CADENCE.md` | 📌 Каданс скрейпа по тарифам × маркетплейсам, волатильностный бэкофф. Ozon-троттл выкл (0/1) — Ozon = WB/YM. |
| `features/scraper-throughput-concurrency.md` | 📌 Пропускная: worker-pool консюмер (№1) + отдельный Ozon-топик (№2). Топология топиков/групп. |
| `SECURITY-HARDENING.md` | 📌 Чек-лист хардинга сервера (ufw/DOCKER-USER, роли PG, requirepass). |
| `SECRET-ROTATION-RUNBOOK.md` | 📌 Ранбук ротации секретов (после инцидента 2026-07-03). |
| `README-deploy.md` | Прод-деплой: nginx+TLS, compose-оверлеи. Инфра актуальна (хост-примеры могут отставать). |

## ✅ Реализовано и в проде (доки-статусы, справочно)

| Док | Статус |
|---|---|
| `WB-SEARCH-STATUS.md` | ✅ Браузерный сайдкар wb-search-miner на проде (горячие 403). |
| `features/search-by-url-ozon-yandex.md` | ✅ Поиск по ссылке-выдаче YM+Ozon включён юзерам. |
| `features/oos-tracking.md` | ✅ Трек товаров без оффера + back_in_stock. |
| `features/wb-ucard-price.md` | ✅ Цена WB через u-card. |
| `features/wb-seller-tracking.md` | ✅ Трек магазина/продавца WB. |
| `ROBOKASSA-INTEGRATION-PLAN.md` | ✅ Робокасса (чеки НПД) + автоплатежи — основной платёжный тракт. |
| `CHECKOUT-PROMO.md` | ✅ Промокоды в платёжном флоу. |
| `SHORT-LINKS.md` | ✅ Резолв коротких ссылок из мобильных приложений (/cc/ и пр.). |
| `YANDEX-STATUS.md` | Журнал YM-интеграции. YM ходит direct без прокси (актуально). |
| `OZON-STATUS.md` | Журнал Ozon-интеграции. Актуальный транспорт — браузерный сайдкар ozon-miner. |

## 🗓 Планы / в работе / дизайн (кода может не быть или частично)

| Док | Статус |
|---|---|
| `PROMO-SHORTS-PLAN.md` | 📌 Продвижение: план 30–60 shorts (VK Клипы/YT/IG/TT), тест-матрица форматов, бюджет $100, пайплайн. |
| `TARIFF-FREE-SEARCH-LINK.md` | Тарифы v2 — в основном реализованы; каданс см. канон `SCRAPE-CADENCE.md`. |
| `SCALING-NOTIFIER-DELIVERY.md` | Дизайн масштабирования notifier (Phase 2, код не тронут; §9 — замер нагрузки). |
| `MAX-INTEGRATION-PLAN.md` | MAX-мессенджер: ветка feat/max-messenger, не задеплоено. |
| `features/honest-price.md` | Спека «честной цены» по истории. |

## 📦 Историческое / устаревшее (НЕ использовать как истину)

| Док | Почему устарел |
|---|---|
| `KANBAN.md` | ⚠️ Задачи ведём в Google Tasks. Доска-журнал, не трогаем как истину. |
| `../ROADMAP.md` | ⚠️ Журнал на 2026-06-11. Заменён Google Tasks + этим индексом. |
| `../tryberrybot-HANDOFF.md` | ⚠️ Хэндофф на 2026-06-04, сильно устарел. |
| `../DEPLOY.md` | ⚠️ Разовая инструкция «Этап 2» (WB-токены). Общий деплой — `README-deploy.md`. |
| `OZON-API-RESEARCH.md` | Ресёрч mobile-API; в проде теперь браузерный сайдкар (см. OZON-STATUS). |
| `YANDEX-WARMED-COOKIES.md` | Спайк прогрева куки через прокси; YM теперь direct без прокси. |
| `URL-SHORTENER-ANALYSIS.md` | Решение «свой shortener не делаем»; итог — резолвер (`SHORT-LINKS.md`). |
| `YOOKASSA-INTEGRATION-PLAN.md` | ЮKassa за флагом; основной тракт — Робокасса. |
| `VK-INTEGRATION-PLAN.md` | План VK; реализовано (VK=TG паритет), дизайн-док историчен. |
| `OZON-API-RESEARCH.md` / `features/scraper-throughput-concurrency.md` §пост-мортем | Ранние диагнозы — читать с датой. |

## Корневые доки

- `../README.md` — репозиторий, обзор.
- `../SECURITY-REVIEW.md` — security review.
