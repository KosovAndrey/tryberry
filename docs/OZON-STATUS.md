# Ozon — статус интеграции (журнал + точка возобновления)

Ветка: **`feat/ozon-scraper`** (всё запушено в origin/GitLab).
Дата последней работы: **2026-06-14**.

## Цель
Получать цену/название/картинку товара Ozon по URL (как WB), для трекинга в боте.
Ограничение пользователя: **без браузера на каждый скрейп**, дёшево, масштабируемо.

## ИТОГ ОДНОЙ СТРОКОЙ
**Рабочая схема найдена и доказана** (FAB пройден, получили 200 с `widgetStates`).
Осталась **одна ~10-минутная проверка с живым мобильным прокси**: один `/track` →
убедиться, что устойчивый парсер достаёт цену со страницы товара. Прокси у Андрея
кончился; берёт ещё ~2 часа на следующий день.

## Что пробовали и что получилось (хронология)

| Подход | Результат | Почему |
|---|---|---|
| Чистый Go (`net/http`/curl) + composer/entrypoint API | ❌ 403 FAB | палится по TLS-отпечатку |
| Go `tls-client` (okhttp/Chrome профиль), аноним | ❌ `fab_nmk_` на датацентре, `fab_chlg_` на мобильном | FAB бьёт по TLS + репутации IP |
| Браузер-майнер (Patchright+Xvfb) добывает `__Secure-ETC` с главной → Go реюзает | ❌ | анонимный ETC с главной FAB не принимает для API товара |
| web-эндпоинт (`entrypoint-api.bx`, Chrome-TLS) + анонимный ETC | ❌ `fab_cp_` (даже при стабильном IP) | токен признан, но запрос режут |
| in-page `fetch` из прошедшей FAB главной | ❌ 403 | FAB валидирует каждую навигацию, не сессию |
| Реальная навигация браузера на `/product/<id>` | ❌ «Antibot Captcha» (и на свежем IP) | фингерпринт Chromium-в-Xvfb + пул палятся |
| **Аккаунт-cookie (путь B): `OZON_COOKIE` из приложения + composer-api + okhttp + RU-мобильный прокси** | ✅ **FAB ПРОЙДЕН** (ошибка стала «price not found», т.е. 200 widgetStates) | доверенная сессия залогиненного аккаунта + хороший IP |
| Тот же аккаунт-cookie, но датацентровый IP сервера (без прокси) | ❌ `fab_chlg_` | **аккаунта мало — FAB также гейтит по IP** |
| xray/wireguard как RU-выход | ❌ (не пробовали до конца) | наш wg/xray — **немецкий**, для Ozon бесполезен |

## Что РАБОТАЕТ и почему
- **Путь B**: cookie-строка из **приложения** (HTTP Toolkit) в `OZON_COOKIE` (содержит
  `__Secure-access-token`/`refresh-token`/`__Secure-user-id`/`abt_data`) +
  эндпоинт `https://api.ozon.ru/composer-api.bx/page/json/v2?url=/products/<id>/?layout_container=pdppage2copy&layout_page_index=1`
  + TLS-профиль `Okhttp4Android13` + заголовки приложения (`x-o3-*`, `MOBILE-GAID`) +
  **RU-мобильный прокси** → FAB пропускает, отдаёт `widgetStates`.
- Эталон — репо `Churkashh/ozon-pinneaples` (тоже под залогиненным аккаунтом).

## Что НЕ работает и почему (ключевые выводы)
- **Анонимно — никак**: FAB упирает в `fab_chlg_`/капчу даже на свежем мобильном IP.
- **Аккаунт-cookie сам по себе недостаточен**: FAB проверяет И сессию, И **IP**.
  Нужен **RU-мобильный/резидентский** IP. Датацентр (в т.ч. IP сервера, наш немецкий wg) — режется.
- **`layout_page_index=2`** грузит вторичный контент (характеристики/описание/рекомендации) —
  **цены там нет**. Цена на `layout_page_index=1`.

## Состояние кода (готово, собрано, тесты зелёные)
- `internal/scraper/ozon.go` — `OzonScraper`:
  - account-режим (`OzonOptions.Cookie` или `AccessToken`/`RefreshToken`) — без Redis/майнера;
  - ETC-режим (через Redis + `ozon-miner`) — запасной;
  - mobile (`composer-api.bx`+okhttp) по умолчанию, web — за флагом;
  - **устойчивый парсер** `widgetStates`: рекурсивный поиск полей `price`/`cardPrice`/
    `originalPrice`, `title`/`text`, `coverImage`/`src` — не завязан на точные имена виджетов;
  - на FAB/неудаче логирует диагностику (`incident`, `minted_ip`/`current_ip`, `priceKey`/`priceRaw`),
    токены/cookie в логи НЕ пишет (только `cookie_len`).
- `ozon-miner/` — сайдкар Patchright+Xvfb (для ETC-пути; для пути B не нужен).
- ENV (в `.env`, секреты вне git): `OZON_COOKIE`, `OZON_PROXY_URL`, `OZON_API_MODE` (mobile),
  опц. `OZON_ACCESS_TOKEN`/`OZON_REFRESH_TOKEN`. Проброшены в `cmd/scraper` и `cmd/bot-worker`,
  в `docker-compose.yml`.
- `/track` в боте скрейпит синхронно через `bot-worker` (не через `pt_scraper`!).

## СЛЕДУЮЩИЕ ШАГИ (когда будет живой мобильный прокси)
1. В `.env` на VPS:
   - `OZON_PROXY_URL=http://<user>:<pass>@<host>:<port>` (приватный мобильный РФ, **ротация 0/sticky**);
   - `OZON_COOKIE='<вся cookie-строка из приложения>'` (если протухла — снять свежую через HTTP Toolkit);
   - `OZON_API_MODE=mobile`.
2. `git pull` → `docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build --force-recreate bot-worker`
3. Проверить лог: `docker logs --tail=5 pt_bot_worker | grep "ozon scraper configured"` → `auth=account-token, proxy=true`.
4. `/track` товара Ozon → `docker logs --tail=10 pt_bot_worker`:
   - ✅ `scraped marketplace=ozon price=…` — **готово, Ozon работает end-to-end**;
   - ⚙️ `price not found; priceKey=…; priceRaw={…}` — прислать строку, докрутить парсер (минуты).
5. После успеха: убрать диагностические дампы, прогнать `/track` на 2-3 разных товарах,
   решить про ETC-майнер (вероятно, для пути B он не нужен — выпилить или оставить за флагом).

## Открытые вопросы / риски
- **Стоимость**: путь B = аккаунт ~125₽ разово + RU-мобильный прокси ~1790₽/мес (**flat**, не
  растёт с объёмом — лучший вариант; C/2captcha и D/Apify растут с числом запросов).
- **Бан аккаунта** под нагрузкой → отдельный/одноразовый аккаунт, низкая частота, пул аккаунтов.
- **Протухание токенов**: у `__Secure-access-token` есть срок; нужен авто-рефреш через
  `refresh-token` (`composer-api.bx/_action/getUserV2` / refresh-флоу) — добавить позже.
- **Прокси sticky обязателен**: при авто-ротации IP меняется между запросами, ETC/сессия к IP
  привязаны → FAB. Управление IP — через ссылку-switch провайдера.

## Безопасность
- Андрей засветил **свою** аккаунт-сессию в чате 14.06 → **разлогинить** (выйти на всех
  устройствах) после тестов; для прода — **отдельный** аккаунт.
- Все секреты — в `.env` (в `.gitignore`), в логи не попадают.

## Полезные ссылки
- Полный технический разбор: `docs/OZON-API-RESEARCH.md`.
- Эталон рабочего рецепта: https://github.com/Churkashh/ozon-pinneaples
