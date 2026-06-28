# MAX-мессенджер: интеграция (полный паритет VK=TG)

Статус: в работе (ветка `feat/max-messenger`). Цель — третий канал доставки
наравне с Telegram и VK. Решения по объёму/ингрессу зафиксированы с владельцем:
**сразу полный паритет** и **webhook → Kafka** (как VK/TG).

## Что такое MAX и чем берём

- Российский мессенджер (max.ru) от VK. Доступен с RU-хостинга напрямую —
  прокси не нужен (как VK; в отличие от Telegram).
- Есть **официальный Go-клиент** `github.com/max-messenger/max-bot-api-client-go`
  (v1.7.1, Go 1.24). Используем его как транспорт (как `vk.Client` — обёртка),
  не пишем HTTP руками.

### Ключевые факты клиента (изучено по исходникам в module cache)
- База: `https://platform-api2.max.ru/`, заголовок `Authorization: <token>`.
- Конструктор: `maxbot.New(token, opts...) (*Api, error)`.
- Отправка: `maxbot.NewMessage().SetUser(userID).SetText(...).SetFormat(schemes.HTML).AddKeyboard(kb)`
  → `api.Messages.Send(ctx, m)`. В ЛС шлём по `SetUser(userID)` (= `max_id`).
- Клавиатуры: inline, кнопки `AddCallback(text, intent, payload)` и
  `AddLink(text, _, url)`. Intent: `schemes.DEFAULT|POSITIVE|NEGATIVE`.
- Ответ на колбэк: `api.Messages.AnswerOnCallback(ctx, callbackID, &schemes.CallbackAnswer{Notification: ...})`.
- Webhook: `api.Subscriptions.Subscribe(ctx, url, types, secret)`; входящий POST
  валидируется заголовком `X-Max-Bot-Api-Secret` (`maxbot.SecretHeader`).
  Парсинг тела: `schemes.Update{UpdateType}` → конкретный тип.
- Типы апдейтов: `MessageCreatedUpdate` (`.Message.Sender.UserId`,
  `.Message.Recipient.ChatId/.ChatType`, `.Message.Body.Text`),
  `MessageCallbackUpdate` (`.Callback.Payload/.CallbackID/.User.UserId`, `.Message`),
  `BotStartedUpdate` (`.User.UserId`, `.ChatId`, **`.Payload` — deeplink!**).
  `UpdateInterface` даёт `GetUserID()`/`GetChatID()`.
- **Deeplink-пейлоад есть** (`BotStartedUpdate.Payload`) → рефералки и привязка
  аккаунтов работают по deep-link, как в Telegram (`?start=CODE`).

## Архитектура (зеркало VK)

```
MAX → webhook POST /max/callback (cmd/api)
     → проверка X-Max-Bot-Api-Secret
     → Kafka topic max-updates (key = user_id, порядок диалога сохраняется)
     → consumer в cmd/bot-worker → max.Bot.HandleUpdate
cmd/notifier: deliverer маршрутизирует по users.notify_channel → max-клиент
```

Подписка на webhook регистрируется на старте cmd/api (Subscriptions.Subscribe),
аналогично тому, как TG ставит webhook. Идемпотентно.

## Доменная модель идентичностей и каналов

Сейчас: `users.telegram_id`, `users.vk_id`, `notify_channel ∈ {auto,tg,vk,both}`.
Добавляем третий канал → модель `both` (=tg+vk) недостаточна.

Решение:
- `users.max_id BIGINT UNIQUE` (= `schemes.User.UserId`).
- `notify_channel` расширяем до `{auto, tg, vk, max, both, all}`:
  - `auto` — куда зарегистрировался;
  - `tg|vk|max` — конкретный канал;
  - `both` — легаси tg+vk (оставляем для совместимости);
  - `all` — все привязанные идентичности.
- `domain.ResolveNotifyTargets(channel, hasTG, hasVK, hasMax) (tg, vk, max bool)` —
  сигнатура расширяется (правит вызовы в notifier/delivery + тесты).
- `NotifyChannelTitle` — добавить MAX, all.
- Цикл кнопки «канал уведомлений» в каждом боте (TG/VK/MAX) предлагает только
  каналы среди привязанных идентичностей + `all` при ≥2.

Миграция `024_max.sql`: + `max_id`, расширить CHECK `chk_user_has_identity`
(`telegram_id OR vk_id OR max_id`), CHECK `chk_user_notify_channel` (+max,+all).

## Привязка аккаунтов (link codes)

Зеркалим VK-механику (`internal/domain/link.go`, redis LinkCodeStore, merge.go):
- направления: `tg2max`, `max2tg`, `vk2max`, `max2vk` (код выдан где → предъявлен где).
- ошибки `ErrMaxAccountBusy` (+ аналоги), слияние через `ComputeMerge`.
- deep-link `?start=link_<code>` тоже поддержать (есть payload).

## Пакет internal/max (порт internal/vk, файл-в-файл)

client.go (обёртка над maxbot.Api), handler.go (роутинг update/callback/start),
track.go, bulktrack.go, search.go, subscription.go, plans.go, admin.go, promo.go,
merge.go, email.go, checkout_promo.go. Тексты/кнопки берём из VK как есть
(HTML-формат MAX поддерживает).

## Точки интеграции (вне internal/max)

- `cmd/api/main.go` — ingestor `/max/callback` + Subscribe + Kafka producer.
- `cmd/bot-worker/main.go` — consumer `max-updates` → max.Bot.
- `cmd/notifier/{main,delivery}.go` — max-sender в deliverer + роутинг.
- `internal/repository/postgres/user.go` — GetByMaxID/UpsertMax/LinkMax/UnlinkMax.
- `internal/repository/redis` — переиспользуем LinkCodeStore (направления — строки).
- `internal/payment` — подключить max-витрину к применению платежей (как VK).
- config + docker-compose: `MAX_BOT_TOKEN`, `MAX_CALLBACK_SECRET`,
  `MAX_WEBHOOK_URL`, `MAX_ADMIN_IDS`.

## Статус реализации (на ветке feat/max-messenger)

Сделано и компилируется/проходит тесты:
- БД (миграция 024), домен (каналы/направления/ResolveNotifyTargets), репозиторий
  (Get/Upsert/Link/UnlinkMax), зависимость + клиент-обёртка.
- Пакет `internal/max` — полный порт VK: трекинг, balk-track, поиск-подписки,
  тарифы/подписки/оплата, промо/рефералка, email, слияние, админка.
- Ingestor (api `/max/callback` + подписка на webhook), консьюмер (bot-worker,
  топик `max-updates`), доставка (notifier), оплата (paymentNotifier в MAX).
- Привязка: MAX выдаёт max2tg/max2vk; TG/VK редимят их (LinkTG/LinkVK по эмитенту).
  MAX редимит tg2max/vk2max (LinkMax). Цикл каналов уведомлений на 3 идентичности.
- Реверс-инициация привязки: кнопки «Привязать MAX» в TG (`profile:linkmax`/
  `profile:relinkmax`, issue tg2max) и VK (`cmdLinkMax`, issue vk2max; «Отвязать MAX» —
  `cmdUnlinkMax`). TG/VK профиль обобщён на 3 идентичности: статус MAX, выбор канала
  уведомлений циклом по привязанным (tg/vk/max/all). MAX_BOT_URL прокинут в оба бота.
- docker-compose (api/bot-worker/notifier) + .env.example.

Отложено (фаст-фоллоу, не блокирует запуск):
- Картинка товара в пуше MAX: используем превью ссылки (SendMessagePhoto шлёт текст,
  ссылка разворачивается клиентом). Полноценный фото-аттач (upload по URL) — позже.

## Открытые вопросы / проверить на проде

- Точный формат deep-link URL MAX (`https://max.ru/<bot>?start=...`?) — уточнить
  при создании бота через MasterBot.
- Лимит rps 30 к platform-api2.max.ru — учесть в нотификаторе (как TG).
- HTML-разметка MAX: проверить поддержку тегов, что шлёт билдер (domain/render).
</content>
</invoke>
