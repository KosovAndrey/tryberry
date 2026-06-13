# Робокасса + подписки (автоплатежи) — план реализации

> Журнал работ по миграции платежей с ЮKassa на Робокассу и внедрению
> рекуррентных платежей (подписок). Ведём как ROADMAP: отмечаем сделанное,
> дописываем решения по ходу.

## Зачем

- **ЮKassa не формирует чеки НПД для самозанятого** — это блокер. Робокасса в
  режиме самозанятого сама регистрирует доход в «Мой налог» и отдаёт чек НПД на
  каждую оплату (в т.ч. на каждое автосписание).
- Хотим **автоплатежи (подписку)** как способ удержания: пользователь
  подключает автопродление — деньги списываются раз в 30 дней без его участия.

## Продуктовые решения (зафиксированы)

1. **Модель — выбор**: на витрине две кнопки — «Оплатить разово» и
   «Подписка с автопродлением». Подписка подсвечена как рекомендуемая.
2. **Подписка чуть дешевле разовой** (nudge), явными числами:

   | Тариф          | Разовая | Подписка |
   |----------------|--------:|---------:|
   | Lite           |     199 |  **189** |
   | Pro            |     499 |  **479** |
   | Reseller Start |     990 |  **940** |
   | Reseller Pro   |    1990 | **1890** |

3. **Промокод (скидка до 50%) применяется только к ПЕРВОМУ платежу.**
   Автопродления идут по обычной подписочной цене (189/479/…). Иначе человек с
   промокодом платил бы вполовину вечно.
4. **Обязательная обвязка подписки (4 защиты от жалоб/чарджбэков/модерации):**
   - экран явного согласия перед первой оплатой (сумма, период «каждые 30 дней»,
     автопродление, как отменить);
   - **лог согласия** (кто, когда, какой текст условий, что нажал) — защита от
     чарджбэка и Роспотребнадзора;
   - кнопка **«Отменить автопродление»** в боте в один тап, работает всегда;
   - **уведомление за 1–3 дня до списания** («спишем X ₽, отменить — тут»).
5. **ЮKassa не выпиливаем сразу** — оставляем за флагом `PAYMENT_PROVIDER`.
   Когда Робокасса стабильно отработает ~2 недели, удалим ЮKassa отдельным
   коммитом.

## Правовой контекст (кратко, для памяти)

- Скидки/промокоды вниз от витринной цены — законны. Жёсткое правило одно: чек
  отражает **фактически уплаченную** сумму. Модерация сайта проверяет оферту и
  реквизиты, не запрещает акции.
- «Только подписка» тоже законна, но мы выбрали модель «выбор» — она безопаснее.
- При **возврате/отмене с возвратом** самозанятый обязан аннулировать чек НПД в
  «Мой налог» (или пробить чек на возврат). MVP: отмена = «отменить автопродление
  с сохранением доступа до конца оплаченного периода», **без возврата денег** →
  чек аннулировать не нужно. Возвраты — отдельный флоу (см. ниже, вне MVP).

---

## Текущая архитектура (что меняем)

```
витрина (TG/VK) → payment.Service.Start → ЮKassa create → confirmation_url
   юзер платит → ЮKassa webhook → cmd/api /yookassa/webhook
   → refetch GetPayment → Kafka topic "payments" (ConfirmedEvent)
   → cmd/bot-worker payment.Applier → MarkSucceeded + продление плана + реф.награда
```

Ключевые файлы:
- `internal/domain/payment.go`, `internal/domain/plan.go` — модель, цены.
- `internal/payment/service.go` — создание платежа (хардкод ЮKassa).
- `internal/payment/yookassa/client.go` — клиент ЮKassa.
- `internal/payment/apply.go`, `events.go` — применение оплаты.
- `cmd/api/main.go` — вебхук `/yookassa/webhook`.
- `cmd/bot-worker/payments.go` — сборка сервиса + консьюмер.
- `internal/repository/postgres/payment.go`, `migrations/016_payments.sql`.

⚠️ **Нейминг**: таблица `subscriptions` УЖЕ занята — это товарные подписки на
снижение цены (ядро бота). Биллинговую таблицу называем **`billing_subscriptions`**,
чтобы не путать.

---

## Особенности Робокассы (vs ЮKassa)

1. **Создание платежа — не API-вызов, а подписанный redirect-URL.** Собираем URL
   `https://auth.robokassa.ru/Merchant/Index.aspx` с параметрами
   `MerchantLogin`, `OutSum`, `InvId`, `Description`, `SignatureValue`,
   (`Receipt`, `Recurring`, `IsTest`). `InvId` — **числовой**, уникальный →
   используем `payments.id` (BIGSERIAL).
2. **Подпись инициализации** (Password1):
   `md5(MerchantLogin:OutSum:InvId:Password1)`; при наличии чека —
   `md5(MerchantLogin:OutSum:InvId:Receipt:Password1)` (Receipt — URL-encoded
   JSON, в подпись идёт ровно та же строка, что в URL).
3. **ResultURL (вебхук) подписан Password2** —
   `md5(OutSum:InvId:Password2)` (+ доп. поля, если включены). В отличие от
   ЮKassa, **подпись и есть доверенный источник** → перечитывать необязательно;
   опционально сверяем через `OpStateExt` (XML-API состояния операции). В ответ
   на ResultURL отдаём `OK{InvId}`.
4. **SuccessURL / FailURL** — редирект юзера после оплаты (на бота).
5. **Чек НПД (самозанятый)**: параметр `Receipt` с `sno` (НПД),
   `items[].payment_method`, `payment_object`, `tax: "none"`. Уточнить точный
   формат в ЛК Робокассы при подключении режима самозанятого.
6. **Рекуррент**:
   - первый платёж создаём с `Recurring=true` (фичу включить в ЛК);
   - последующие — **server-to-server** POST на
     `https://auth.robokassa.ru/Merchant/Recurring` с `MerchantLogin`,
     `InvoiceID` (новый = новый `payments.id`), `PreviousInvoiceID`
     (= InvId первого платежа), `OutSum`, `SignatureValue`. Без редиректа.
   - результат списания приходит на тот же **ResultURL**.
7. **Тестовый режим**: `IsTest=1` + тестовые пароли.

---

## Изменения по слоям

### 1. Домен / цены
- `internal/domain/plan.go`: добавить поле `SubPriceRub int` в `Plan`, заполнить
  для платных планов (189/479/940/1890). 0 → подписка для плана недоступна.
- Хелперы (новый файл `internal/domain/billing.go` или в `payment.go`):
  - расчёт суммы первого платежа подписки (sub-цена, затем промо-скидка поверх);
  - сумма автопродления = sub-цена без промо.
- Статусы подписки: `active`, `canceled` (отменена, доступ до конца периода),
  `past_due` (списание не прошло, в ретраях), `expired`.

### 2. Провайдер платежей — абстракция
Ввести интерфейс, чтобы `Service` не зависел от конкретного провайдера и работал
флаг `PAYMENT_PROVIDER`:

```go
// internal/payment/provider.go
type Provider interface {
    // Checkout — ссылка на оплату + внешний id (для ЮKassa — yk id;
    // для Робокассы внешний id == наш InvId, network-вызова нет).
    Checkout(ctx, params CheckoutParams) (url, externalID string, err error)
    // ChargeRecurring — автосписание по сохранённой связке (только Робокасса).
    ChargeRecurring(ctx, params RecurringParams) error
    Name() string
}
```
- `internal/payment/yookassa/` — адаптировать существующий клиент под интерфейс.
- `internal/payment/robokassa/` — новый клиент: сборка/подпись URL, проверка
  ResultURL-подписи, `ChargeRecurring`, формирование `Receipt` (НПД). Покрыть
  тестами подпись и формат Receipt (как `yookassa/client_test.go`).

### 3. Миграция БД (`migrations/018_billing_subscriptions.sql`)
- `payments`: добавить
  - `provider TEXT NOT NULL DEFAULT 'yookassa'`,
  - `kind TEXT NOT NULL DEFAULT 'onetime'` (`onetime|subscription_initial|subscription_renewal`),
  - `billing_subscription_id BIGINT NULL REFERENCES billing_subscriptions(id)`.
  - `yk_payment_id` остаётся (nullable) — для Робокассы внешний id = `id`.
- новая `billing_subscriptions`:
  ```
  id BIGSERIAL PK
  user_id BIGINT NOT NULL REFERENCES users(id) ON DELETE CASCADE
  plan TEXT NOT NULL
  status TEXT NOT NULL DEFAULT 'active'  -- active|canceled|past_due|expired
  amount_kopecks BIGINT NOT NULL          -- сумма автопродления (sub-цена)
  recurring_invoice_id BIGINT NOT NULL    -- PreviousInvoiceID = InvId первого платежа
  next_charge_at TIMESTAMPTZ NOT NULL
  pre_notice_sent_at TIMESTAMPTZ          -- когда отправили предупреждение
  fail_count INT NOT NULL DEFAULT 0
  last_payment_id BIGINT
  created_at, updated_at, canceled_at TIMESTAMPTZ
  UNIQUE (user_id) WHERE status='active'  -- одна активная подписка на юзера
  ```
- новая `subscription_consents` (лог согласия):
  ```
  id, user_id, plan, amount_kopecks, terms_version TEXT,
  platform TEXT, created_at
  ```

### 4. Витрина (TG/VK)
- Две кнопки на платном тарифе: «Оплатить разово {price}» / «Подписка {sub} с
  автопродлением».
- По кнопке подписки → **экран согласия** (текст условий + версия) → кнопка
  «Подтверждаю, оформить» → пишем `subscription_consents` → создаём первый платёж
  с `Recurring=true`.
- Раздел «Моя подписка»: статус, дата следующего списания, кнопка
  «Отменить автопродление».
- Файлы: обработчики витрины в `internal/telegram/…` и `internal/vk/…`
  (найти текущие хендлеры показа тарифов/оплаты).

### 5. Вебхук Робокассы (`cmd/api`)
- Эндпоинт `/robokassa/result` (POST, form-urlencoded):
  - проверить `SignatureValue` (Password2);
  - `InvId` → `payments.id`; определить `kind`;
  - опц. сверка `OpStateExt`;
  - публикация `ConfirmedEvent` в Kafka `payments` (расширить событие полем
    `Provider`/`PaymentID` или перейти на `payments.id` как ключ — см. ниже);
  - ответ `OK{InvId}`.
- `/robokassa/success` и `/robokassa/fail` — редирект юзера в бота (deep link).
- ЮKassa-вебхук остаётся, монтируется по флагу провайдера.

### 6. Применение оплаты (`internal/payment/apply.go`)
- `ConfirmedEvent`: сделать провайдеро-нейтральным — нести `PaymentID int64`
  (= `payments.id`) вместо `YKPaymentID`, либо добавить `Provider`. `Applier`
  читает строку `payments` по id.
- При `kind=subscription_initial`: после продления плана **создать
  `billing_subscriptions`** (recurring_invoice_id = payments.id,
  next_charge_at = expires_at − небольшой запас, amount = sub-цена).
- При `kind=subscription_renewal`: продлить план, обновить
  `next_charge_at`, обнулить `fail_count`, `status=active`.
- Промокод гасим только на initial/onetime (на renewal промо нет).

### 7. Шедулер автосписаний (bot-worker)
Переиспользовать существующий периодический раннер (тот, что крутит
plan-reconciler из `subscription.go`), добавить два прохода:
- **Pre-notice**: подписки с `next_charge_at` в ближайшие 1–3 дня и
  `pre_notice_sent_at IS NULL` → уведомить, проставить `pre_notice_sent_at`.
- **Charge**: подписки `status=active AND next_charge_at <= now` → создать
  `payments` row (`kind=subscription_renewal`, InvId=id) → `ChargeRecurring`
  (PreviousInvoiceID = recurring_invoice_id). Результат придёт на ResultURL.
- **Retry/dunning**: если списание не прошло (приходит fail или таймаут) →
  `fail_count++`, перенести `next_charge_at` (например +1д, +3д), уведомить;
  после N неудач → `status=past_due` → доступ истекает по плану, подписка
  `expired`, уведомить «продлите вручную».

### 8. Отмена
- Кнопка «Отменить автопродление» → `billing_subscriptions.status=canceled`,
  `canceled_at=now`. **Доступ сохраняется до конца оплаченного периода** (план не
  трогаем). Следующего списания не будет.
- (Вне MVP) Возврат денег → `ChargeRecurring`-refund / API возврата Робокассы +
  **аннулирование чека НПД** в «Мой налог».

### 9. Конфиг / флаги
```
PAYMENT_PROVIDER=robokassa|yookassa     # дефолт на период обкатки — обсудить
ROBOKASSA_MERCHANT_LOGIN=
ROBOKASSA_PASSWORD1=                     # подпись инициализации (секрет, вне git)
ROBOKASSA_PASSWORD2=                     # проверка ResultURL (секрет, вне git)
ROBOKASSA_IS_TEST=0|1
ROBOKASSA_RESULT_URL= / SUCCESS_URL= / FAIL_URL=
ROBOKASSA_NPD=1                          # режим самозанятого (формат Receipt)
```
Секреты — по [[wg-egress-deploy-gotchas]] вне git. Egress: проверить, что
auth.robokassa.ru доступен через прокси (RU-egress) так же, как ЮKassa.

---

## Порядок работ (этапы)

1. ✅ **Каркас + цены** (ветка `feat/robokassa-subscriptions`): `SubPriceRub`
   (189/479/940/1890), доменная модель `BillingSubscription`/`SubscriptionConsent`
   + статусы (`internal/domain/billing.go`), миграция `018_billing_subscriptions.sql`
   (колонки `provider`/`kind`/`billing_subscription_id` в payments + таблицы
   `billing_subscriptions`, `subscription_consents`), интерфейс `payment.Provider`
   (`internal/payment/provider.go`), адаптер ЮKassa (`yookassa_provider.go`),
   `Service` переведён на `Provider`, флаг `PAYMENT_PROVIDER` (`setupProvider` в
   `cmd/bot-worker/payments.go`). Поведение ЮKassa не изменилось; build/vet/тесты
   зелёные.
2. ✅ **Робокасса-клиент** (`internal/payment/robokassa/client.go`): `BuildPaymentURL`
   (подпись `MerchantLogin:OutSum:InvId[:Receipt]:Password1`, Receipt URL-encoded
   одинаково в подписи и URL), `VerifyResult` (Password2, case-insensitive),
   `ChargeRecurring` (S2S POST `/Recurring`), чек НПД (`sno=npd`, `tax=none`),
   `IsTest`, конфигурируемый hash (md5/sha256/sha512). Тесты на подписи —
   `client_test.go`. Адаптер `payment.NewRobokassaProvider` (`robokassa_provider.go`)
   + ветка `robokassa` в `setupProvider` (env `ROBOKASSA_MERCHANT_LOGIN/PASSWORD1/
   PASSWORD2/IS_TEST/SNO/HASH_TYPE/NPD`). build/vet/тесты зелёные.
   ⚠️ Формат чека НПД (`sno`/`tax`/`payment_object`) — проверить против ЛК
   Робокассы при подключении.
3. ✅ **Разовый платёж через Робокассу** end-to-end (без рекуррента): витрина →
   `Service.Start` → `provider.Checkout` (подписанный redirect) → `/robokassa/result`
   (проверка подписи Password2, `ConfirmInfo` по InvId, сверка суммы) → Kafka →
   Applier. Сделано провайдеро-нейтральным: `ConfirmedEvent.PaymentID` (= payments.id)
   вместо `YKPaymentID`, `MarkSucceeded` ищет по `id`; YK-хендлер публикует
   `payment_id` из metadata. Вебхук Робокассы в `cmd/api` отдаёт `OK{InvId}`.
   build/vet/тесты зелёные. Чек НПД проверить на живой оплате (см. этап 6).
4. **Подписка — первый платёж**: экран согласия + лог, `Recurring=true`,
   создание `billing_subscriptions`, раздел «Моя подписка» + отмена.
5. **Шедулер автосписаний** + pre-notice + retry/dunning.
6. **Обкатка ~2 недели**, метрики (успешные/неуспешные списания, активные
   подписки, чарджбэки). Затем — удаление ЮKassa отдельным коммитом.

## Открытые вопросы (уточнить при подключении Робокассы)
- Точный JSON `Receipt` для режима НПД (sno/tax/payment_object).
- Активирован ли рекуррент в ЛК; лимиты/комиссии на автосписания.
- Доступность `auth.robokassa.ru` через RU-egress прокси.
- Дефолт `PAYMENT_PROVIDER` на период миграции (robokassa сразу или после
  ручной проверки разового платежа).
