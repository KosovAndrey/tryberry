# Платёжный флоу ЮKassa — план и решения

Статус: в работе (ветка `feat/monitoring-phase2` → выделить `feat/yookassa`).
Автор разбора: 2026-06-13. Контекст — ROADMAP §A «Монетизация — ЮKassa».

## Что уже было готово (крючки), а что — нет

ROADMAP описывал `payments`-таблицу и вебхук как «готовые крючки», но **по факту
их не было**: миграции заканчивались на `015`, платёжного эндпоинта в `cmd/api`
не было. Реально готовые крючки:

- `UserRepo.SetPlan` + восстановление трекинга при покупке (reconciler);
- discount-промокоды (`PromoKindDiscount`, `promo_claims` от фарма) — но **без
  хранения «ожидающей скидки»** у юзера: `/promo <discount>` сейчас только
  показывает «у тебя скидка N%», ни к чему не привязывая;
- реферальное событие `paid` (`ReferralEventPaid`, +30 дней) — в схеме есть,
  начисления нет;
- кнопки-заглушки `plan:buy:*` (TG, `internal/telegram/plans.go`) и
  `{"cmd":"buy","k":<plan>}` (VK, `internal/vk/handler.go:316`).

## Архитектура потока

Следуем разделению сервисов: `api` = ингестор (только приём и валидация),
`bot-worker` = обработка и ответы (у него есть и TG-, и VK-боты, и все репозитории).

```
[создание платежа]                         [подтверждение]
bot-worker (callback plan:buy / cmd:buy)   ЮKassa → POST /yookassa/webhook (cmd/api)
  → PaymentRepo.Create (status=pending)      → re-fetch GET /payments/{id} (НЕ доверяем телу)
  → YooKassa.CreatePayment (Idempotence-Key) → если succeeded: publish Kafka topic `payments`
  → сохранить yk_payment_id                     (key = user_id) {yk_payment_id}
  → отдать confirmation_url юзеру            bot-worker (consumer payment-workers)
                                              → PaymentRepo.MarkSucceeded (идемпотентно)
                                              → продлить план + погасить discount + реф. paid
                                              → уведомить юзера (TG/VK)
```

Почему вебхук НЕ применяет план сам, а шлёт в Kafka: применение требует уведомить
юзера в нужном канале (TG/VK) — это умеет только bot-worker. `api` остаётся чистым
ингестором. Топик `payments`, ключ = `user_id` (порядок на юзера), группа
`payment-workers`.

### Идемпотентность (вебхуки ЮKassa повторяются)

- `payments.yk_payment_id UNIQUE` + переход статуса под `UPDATE ... WHERE
  status='pending' RETURNING ...`: применяет план только первый, кто перевёл
  pending→succeeded. Повторы и переотправки Kafka → 0 строк → no-op.
- На создание — `Idempotence-Key` (UUID) в заголовке YooKassa: повтор POST не
  плодит второй платёж.

## Миграция 016_payments.sql

```sql
CREATE TABLE payments (
    id              BIGSERIAL PRIMARY KEY,
    user_id         BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    yk_payment_id   TEXT UNIQUE,                 -- id в ЮKassa (идемпотентность вебхуков)
    idempotence_key TEXT NOT NULL UNIQUE,        -- наш ключ на create
    plan            TEXT NOT NULL,
    days            INT  NOT NULL DEFAULT 30,
    amount_kopecks  BIGINT NOT NULL,             -- сумма к оплате (со скидкой)
    promo_code_id   BIGINT REFERENCES promo_codes(id), -- discount-код, если был
    status          TEXT NOT NULL DEFAULT 'pending',   -- pending|succeeded|canceled
    created_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    paid_at         TIMESTAMPTZ
);
CREATE INDEX idx_payments_user ON payments(user_id);
```

## Продление плана при покупке (domain.ApplyPurchase)

Зеркало `ApplyGrantPromo`, но покупка всегда проходит (без `ErrPromoPlanConflict`):
- тот же план активен → от `plan_expires_at` + days;
- free/trial/истёкший/другой план → `now + days`.
План пишем по `users.id` (как `RedeemGrant`), а не `SetPlan(telegram_id)` —
это работает и для VK-only юзеров (нет telegram_id).

## Discount-промокоды

«Ожидающая скидка» — в Redis (`redisrepo.DiscountStore`, TTL ~24ч), ключ =
`users.id` → `{code_id, pct}`. Кладём при `/promo <discount>` (и VK-аналоге).
На `plan:buy` читаем → уменьшаем `amount_kopecks` → пишем `promo_code_id` в
платёж. На `succeeded` — `PromoRepo.RedeemDiscount(code_id, user_id)` (инкремент
`used_count` + `promo_redemptions` + `promo_claims`, БЕЗ смены плана) и чистим Redis.

## Реферальное событие paid

После успешного применения — `ReferralRepo.ReferrerOf(buyerID)`; если есть реферер
→ `ApplyReferralReward(referrer, ReferralPaidRewardDays=30)` + `GrantReward(...,
ReferralEventPaid, ...)` (идемпотентно: UNIQUE (referee, event) + потолок).
Уведомление рефереру — как у `activated`.

## Конфиг (env, секреты вне git — см. memory wg-egress-deploy-gotchas)

- `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY` — Basic-auth к API.
- `YOOKASSA_RETURN_URL` — куда вернуть юзера после оплаты (ссылка на бота).
- `YOOKASSA_WEBHOOK_ENABLED` — монтировать ли `/yookassa/webhook` в api.
- vk.com без прокси, **а api ЮKassa — наружу через `HTTPS_PROXY`** (RU-egress).
  Проверить, что ЮKassa доступна с egress-IP; если нет — ходить напрямую.

## Чеки 54-ФЗ — РЕШЕНО (путь 2: передаём receipt сами), СДЕЛАНО 2026-06-13

Владелец самозанятый → чек обязателен, передаём в каждом платеже. Реализовано:
- миграция `017_user_email` (`users.email`), `UserRepo.GetEmail/SetEmail`;
- перед ПЕРВОЙ оплатой бот отдельным сообщением просит email с пояснением
  («по 54-ФЗ нужен чек, ЮKassa отправит его на этот адрес») — FSM в Redis
  (`email_fsm` TG / `vk_email_fsm` VK); email сохраняется, повторно не спрашиваем;
  любая команда/кнопка отменяет ввод; валидация — `domain.ValidEmail` (net/mail);
- `Service.Start(…, email)` кладёт `receipt`: одна позиция-услуга на всю сумму,
  `payment_subject=service`, `payment_mode=full_prepayment`, `vat_code` из env
  `YOOKASSA_VAT_CODE` (дефолт **1 = «без НДС»**, самозанятый — не плательщик НДС).

Путь «Чеки от ЮKassa» в ЛК включать НЕ нужно — чек идёт в запросе платежа.

## Telegram Stars (XTR) — вне scope этой итерации (опц. второй канал позже).

## Чек-лист реализации — СДЕЛАНО (код, 2026-06-13)

- [x] migration 016_payments.sql
- [x] internal/payment/yookassa: Client (CreatePayment, GetPayment) + тест (RoundTripper-стаб)
- [x] PaymentRepo: Create, SetYKID, MarkSucceeded (идемпотентный apply-tx)
- [x] PromoRepo.RedeemDiscount (без смены плана)
- [x] domain.ApplyPurchase / DiscountedKopecks / KopecksToRubString (+ тесты)
- [x] redisrepo.DiscountStore (put/get/del pending-скидки)
- [x] internal/payment: Service (создание платежа) + Applier (применение) + Notifier
- [x] TG: plan:buy:* → создание платежа → URL-кнопка (фолбэк на заглушку, если payments==nil)
- [x] VK: cmd:buy → создание платежа → open_link + ссылка текстом
- [x] промокод-discount: сохранение «ожидающей скидки» в TG и VK
- [x] cmd/api: /yookassa/webhook (re-fetch статуса + publish Kafka `payments`)
- [x] cmd/bot-worker: consumer `payments` → MarkSucceeded → discount + реф.paid → уведомление
- [x] метрики: payments_created/succeeded/revenue_kopecks
- [x] чеки 54-ФЗ: migration 017_user_email, GetEmail/SetEmail, сбор email (FSM TG+VK),
      receipt в платеже, vat_code из env, domain.ValidEmail
- [x] env в docker-compose.yml (api + bot-worker), nginx /yookassa/webhook, NO_PROXY api.yookassa.ru
- [x] go build ./... + go vet + go test ./... — зелено
```

## Деплой (когда будут секреты)

1. В `.env` на VM (вне git): `YOOKASSA_SHOP_ID`, `YOOKASSA_SECRET_KEY`,
   опц. `YOOKASSA_RETURN_URL` (дефолт `https://t.me/TryBerryBot`),
   опц. `YOOKASSA_VAT_CODE` (дефолт `1` = без НДС, самозанятый).
2. Применить миграции: `make migrate` (goose up → 016_payments, 017_user_email).
3. Пересобрать/поднять `api`, `bot-worker`, перезагрузить `nginx` (новый location).
4. В ЛК ЮKassa указать URL вебхука: `https://tryberry.ru/yookassa/webhook`,
   события `payment.succeeded` (и `payment.canceled` по желанию).
5. Тест боевым рублём: `/plans` → «Оплатить» → ввести email → оплата → ждём
   уведомление «✅ Оплата прошла», чек на email, и проверяем `users.plan` +
   строку `payments` (status=succeeded).
   Метрики: `payments_succeeded_total`, `payment_revenue_kopecks_total`.

