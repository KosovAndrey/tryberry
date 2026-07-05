# Нагрузочный тест прода: ~1000 синтетических юзеров

Двойная цель:

1. **Нагрузка.** Проверить конвейер scheduler → Kafka → scraper → БД → notifier
   на масштабе ~1000 юзеров ДО прихода реальных — найти узкие места и получить
   «сигнал нагрузки» для отложенной задачи Phase 2 масштабирования доставки
   (docs/SCALING-NOTIFIER-DELIVERY.md).
2. **Данные для графиков.** Бэкфилла истории цен нет (кроме WB) — график
   наполняется только форвард-накоплением. Синтетики трекают **реальные
   популярные товары**, так что каждый день теста = плюс день истории. Когда
   придут реальные юзеры и добавят топовый товар, график у них будет сразу.

## Дизайн

«1000 юзеров» для этой системы — это не сообщения боту (единицы RPS в пике),
а **состояние базы**: юзеры с тарифами и подписки на товары. Сидим их напрямую
в PG — дальше система нагружает себя сама по штатным кадансам тарифов
(free 60м / lite 30м / pro 15м; Ozon ×2, пол 20м).

- `users.is_synthetic` (миграция 025) — флаг синтетика. Фейковые идентичности
  в диапазоне от 9_100_000_000_000_000_000 (`synthTGBase`) — коллизии с
  реальными TG/VK/MAX id исключены.
- **Товары реальные** — собираются обходом поисковой выдачи по ~45 популярным
  запросам (`cmd/seed-loadtest collect`). Всё direct, прокси нет (отменены
  2026-07-03), стоимость обхода — ноль.
- **Раздача по Zipf**: все трекают одни и те же топовые позиции, хвост
  достаётся немногим. Уникальных товаров получается ~2–4 тыс. — именно их
  число определяет объём скрейпа; несколько pro-подписчиков на популярном
  товаре разгоняют его до 15-минутного каданса (MIN по подписчикам).
- Распределение тарифов: 75% free / 15% lite / 10% pro (плюс-минус — веса в
  `cmd/seed-loadtest/seed.go`). Каналы: 60% tg / 20% vk / 10% max / 10% связки.

### Доставка алертов → тест-аккаунты

Синтетикам нельзя слать на фейковые id (поток 400-х от Telegram API), но и
глушить доставку целиком нельзя — тогда не тестируется самое главное.
Решение — **редирект с сэмплированием** в notifier (`cmd/notifier/delivery.go`):

- `SYNTH_REDIRECT_TG_IDS` / `_VK_IDS` / `_MAX_IDS` — CSV chat_id реальных
  тест-аккаунтов (2 TG + 2 VK + 1 MAX; основной TG-аккаунт НЕ указывать).
- Форвардится **каждый N-й синтетик** (`SYNTH_REDIRECT_SAMPLE_N`, дефолт 10),
  детерминированно по `users.id` — один синтетик всегда попадает в один и тот
  же тест-чат. Остальные дропаются на самом send'е с метрикой
  `notifications_delivered_total{channel="synth",status="skipped"}` — путь
  «решение → pending_alerts → флашер» они прогружают полностью.
- Зачем сэмплировать: в жизни алерты 1000 юзеров размазаны по 1000 чатам, в
  тесте слились бы в 5; у TG лимит ~1 msg/s на чат — без сэмплирования notifier
  упёрся бы в искусственный hot-chat затык, которого в реальности нет.
- Канальность сохраняется: фейковый TG → тест-TG, фейковый VK → тест-VK,
  фейковый MAX → тест-MAX (заодно закрывается «глянуть вид алерта в MAX»).
- Без env-переменных синтетики просто дропаются; поведение для реальных юзеров
  не меняется ни в каком режиме.

## Запуск (прод)

```bash
# 0) Смержить ветку, накатить миграцию 025 ДО деплоя (новый notifier читает
#    колонку is_synthetic). PG-порт после хардинга наружу не торчит, а образ
#    goose с ghcr не тянется (denied) — простые миграции катим руками psql
#    от владельца базы (user, не tryberry_app: нужен ALTER TABLE) + строка
#    в goose_db_version, чтобы учёт версий не разъехался:
docker compose exec postgres psql -U user -d tryberrybot \
  -c "ALTER TABLE users ADD COLUMN IF NOT EXISTS is_synthetic BOOLEAN NOT NULL DEFAULT FALSE;" \
  -c "CREATE INDEX IF NOT EXISTS idx_users_synthetic ON users (id) WHERE is_synthetic;" \
  -c "INSERT INTO goose_db_version (version_id, is_applied) VALUES (25, true);"

#    Затем деплой notifier с SYNTH_REDIRECT_* в .env:
#    SYNTH_REDIRECT_TG_IDS=<tg_id_1>,<tg_id_2>
#    SYNTH_REDIRECT_VK_IDS=<vk_id_1>,<vk_id_2>
#    SYNTH_REDIRECT_MAX_IDS=<max_id_1>
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --build notifier

# 1) Собрать one-off образ утилиты
docker build --build-arg SERVICE=seed-loadtest -t tryberry-seed .

# 2) Собрать товары (WB+YM+Ali direct; Ozon добавится, если передать OZON_BROWSER_URL).
#    Сеть — как у остальных сервисов (проверить: docker network ls | grep tryberry).
#    --user: образ работает от юзера app, иначе permission denied на ./loadtest.
#    REDIS_URL: в .env только REDIS_PASSWORD (URL собирает compose) — без него
#    NOAUTH и пустой пул WB-токенов, весь WB выпадет из сбора.
mkdir -p loadtest
docker run --rm --network tryberrybot_default --env-file .env \
  --user "$(id -u):$(id -g)" \
  -e REDIS_URL="redis://:$(grep '^REDIS_PASSWORD=' .env | cut -d= -f2-)@redis:6379" \
  -e OZON_BROWSER_URL=http://ozon-miner:8095 \
  -v "$PWD/loadtest:/data" tryberry-seed \
  collect -out /data/products.jsonl
# ~45 запросов × 4 МП, резюмируемо (повторный запуск докачивает пропущенное)

# 3) Засеять (сначала dry-run без -yes — покажет объёмы; на первый прогон
#    рекомендуется -users 50, убедиться в доставке, cleanup, потом 1000)
docker run --rm --network tryberrybot_default --env-file .env \
  --user "$(id -u):$(id -g)" \
  -e DATABASE_URL="postgres://tryberry_app:<APP_DB_PASSWORD>@postgres:5432/tryberrybot?sslmode=disable" \
  -v "$PWD/loadtest:/data" tryberry-seed \
  seed -in /data/products.jsonl -users 1000 -yes
```

`DATABASE_URL` в .env отсутствует (собирается в compose из APP_DB_PASSWORD) —
для one-off контейнера передать явно, как выше. Аналогично можно подставить
из .env без ручного копирования:
`-e DATABASE_URL="postgres://tryberry_app:$(grep '^APP_DB_PASSWORD=' .env | cut -d= -f2-)@postgres:5432/tryberrybot?sslmode=disable"`.

## Что смотреть (1–2 недели, Grafana)

- `pending_alerts_depth` и lag consumer-group price-events — триггеры Phase 2;
- `scrape_requests_total` по маркетплейсам/статусам: у YM следить за капчёй
  (рост RPS), у Ozon — за age-gate/blocked;
- `notifications_delivered_total{channel="synth"}` vs `{channel="tg|vk|max"}` —
  объём потока алертов и доля доставленного;
- p95 длительности скрейпа, CPU/RAM контейнеров (scraper, notifier, postgres),
  рост диска: `price_history` — главный писатель (заодно фактура для задачи
  «оценить память и что реплицировать»);
- вид алертов в MAX/VK/TG на тест-аккаунтах (текст, превью, график по ссылке).

## Уборка

```bash
docker run --rm --network tryberrybot_default \
  -e DATABASE_URL="postgres://tryberry_app:$(grep '^APP_DB_PASSWORD=' .env | cut -d= -f2-)@postgres:5432/tryberrybot?sslmode=disable" \
  tryberry-seed cleanup -yes
```

Удаляет синтетиков, их подписки, notifications и pending_alerts. **Товары и
price_history остаются** — это и есть накопленные данные для графиков. После
уборки товары без подписчиков перестают скрейпиться (планировщик берёт только
товары с активными подписками) — история замирает, пока товар не добавит
реальный юзер. Если хочется продолжать копить историю по топ-товарам после
теста — оставить часть синтетиков (например, 50 free-юзеров) или сделать
отдельного служебного юзера-«коллектора» (решить по итогам теста).

SYNTH_REDIRECT_* из .env после теста убрать (redeploy notifier).

## Известные ограничения

- Повторный `seed` при живых синтетиках отказывает (дубли фейковых id) —
  сначала `cleanup`.
- lite/pro синтетикам ставится plan_expires_at = +30 дней; если тест длится
  дольше, реконсайлер начнёт их даунгрейдить (само по себе тоже нагрузочный
  сценарий, но помнить).
- `collect` для Ozon требует сайдкар ozon-miner (`OZON_BROWSER_URL`); без него
  Ozon просто не участвует.
- Дайджест (DIGEST_ENABLED) шлёт по всем юзерам — при включённом дайджесте
  синтетики тоже попадут в выборку, доставка уйдёт через тот же редирект.
