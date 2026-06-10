# Security Review — tryberrybot

Ревью всего проекта (Go ~9k LOC + SQL/shell/nginx). Дата: 2026-06-10.

## Статус
- Ветка: `security/webhook-auth-and-idor` (запушена на origin/GitLab, в `main` ещё **не** смержена).
- Коммит: `e2e49e4` — 13 файлов, +157/−45.
- Сборка проверена в `golang:1.26-alpine`: `go build ./...` → **BUILD_OK**.
- Тесты: пакеты подписок зелёные (`domain`, `searchsub`, `search-worker`). Красные `TestSelectProductPauses` и `TestSelectSearchRestores` в `cmd/notifier` — **предсуществующие**, не от наших правок (устаревшие ожидания лимитов тарифов после коммита `f60226b`, наш коммит этот пакет не трогает). Деплой не блокируют.

## Найденные уязвимости

### 1. HIGH — спуфинг апдейтов через публичный `/webhook` (auth bypass)
- **Где:** `cmd/api/main.go` (обработчик `/webhook`), `internal/telegram/receiver.go` (`SetWebhook`).
- **Суть:** эндпоинт принимал любой JSON-апдейт без аутентификации (secret-token Telegram не использовался, заголовок `X-Telegram-Bot-Api-Secret-Token` не проверялся). Обработчик монтировался всегда, даже в polling-режиме. Доверие в боте — по `message.from.id`, а админ определяется по нему же.
- **Эксплойт:** `POST https://tryberry.ru/webhook` с поддельным `from.id = <admin>` и `text:"/grant <self> unlimited"` → эскалация привилегий; либо `from.id = <жертва>`, `chat.id = <свой>`, `/list` → утечка чужих подписок.

### 2. MEDIUM — IDOR в callback'ах подписок
- **Где:** `internal/telegram/search.go` (`callbackUntrackSearch`), `internal/telegram/track.go` (`handleTrackTriggerCallback`, `handleTrackThreshold`), `internal/telegram/bot.go` (`handleUntrack`, `callbackUntrack`).
- **Суть:** `Deactivate(id)` и `SetTrigger(id, …)` работали по ID подписки без фильтра по владельцу (`WHERE id=$1`). В связке с уязвимостью №1 — перебор ID и порча/отмена чужих подписок.

## Что проверено и чисто
- SQL-инъекций нет — всё параметризовано (`pgx`, `$1,$2,…`).
- TLS/SSRF: нет `InsecureSkipVerify`, прокси из доверенных env; пользовательские ссылки валидируются/нормализуются до запроса.
- Command injection: нет (Python-майнер — статичный JS в `page.evaluate`, shell-скрипты без недоверенного ввода).
- HTML-ответы экранируются (`htmlEscape`).

## Сделано ✅

### Уязвимость №1 — аутентификация вебхука
- `receiver.go`: `SetWebhook(url, secretToken)` теперь регистрирует `secret_token` через `MakeRequest("setWebhook", …)` (поля `SecretToken` нет в tgbotapi v5.5.1).
- `cmd/api/main.go`:
  - обработчик `/webhook` сверяет `X-Telegram-Bot-Api-Secret-Token` через `subtle.ConstantTimeCompare`; несовпадение → `401`.
  - обработчик монтируется **только** при `WEBHOOK_ENABLED=true` (в polling-режиме `/webhook` → `404`).
  - fail-fast на старте: `WEBHOOK_ENABLED=true` без `TELEGRAM_WEBHOOK_SECRET` → ошибка запуска.
- Конфиги: `TELEGRAM_WEBHOOK_SECRET` добавлен в `.env.example`, `.env.prod.example`, `docker-compose.yml` (сервис `api`).

### Уязвимость №2 — IDOR
- Репозитории: `SubscriptionRepo.Deactivate(id, userID)`, `SubscriptionRepo.SetTrigger(id, userID, …)`, `SearchSubscriptionRepo.Deactivate(id, userID)` — добавлен `AND user_id = $N`.
- Интерфейсы `internal/repository/repository.go`, `internal/repository/search.go` обновлены.
- Все вызывающие хендлеры резолвят владельца по `from.id` и прокидывают `user.ID`.

## Осталось / к деплою ⏳
- [x] **Собрать и прогнать тесты** — `BUILD_OK` в `golang:1.26-alpine`. Падают только 2 предсуществующих теста `cmd/notifier` (см. «Статус»).
- [ ] **Смержить MR** `security/webhook-auth-and-idor` → `main` в GitLab.
- [ ] **Задеплоить в прод** (затронуты `api` и `bot-worker`; остальные сервисы изменённую логику не вызывают):
  ```
  git fetch origin && git checkout security/webhook-auth-and-idor && git pull --ff-only origin security/webhook-auth-and-idor
  docker compose -f docker-compose.yml -f docker-compose.prod.yml build api bot-worker
  docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d api bot-worker
  ```
  Проверка: `docker logs --tail=50 pt_api` («polling mode enabled»), `docker logs --tail=50 pt_bot_worker`; в Telegram — `/list`+отмена, поиск-подписки, `/grant` (для админа).
  Миграции БД **не нужны** (схема не менялась).
- [ ] **Секрет нужен только при переходе на webhook:** прод сейчас на polling (`docker-compose.prod.yml: WEBHOOK_ENABLED=false`), `api` стартует без него. Если включат `WEBHOOK_ENABLED=true` — добавить в прод-`.env`: `echo "TELEGRAM_WEBHOOK_SECRET=$(openssl rand -hex 32)" >> .env` (иначе `api` намеренно не стартует — fail-fast).
- [ ] **(Defense-in-depth, опционально)** ограничить в nginx `location = /webhook` диапазонами Telegram (`149.154.160.0/20`, `91.108.4.0/22`).

## Не из этого ревью (отдельно, не блокирует)
- [ ] Починить устаревшие тесты `cmd/notifier` (`TestSelectProductPauses`, `TestSelectSearchRestores`) — ожидания лимитов разошлись с текущим `domain.Plans` после `f60226b`. Обновить ожидаемые значения под актуальный каталог тарифов.

## Не уязвимости (зафиксировано, чтобы не возвращаться)
- Логирование URL/не-PII — ок.
- Хранение секретов на диске вне git — вне скоупа (управляется отдельно).
