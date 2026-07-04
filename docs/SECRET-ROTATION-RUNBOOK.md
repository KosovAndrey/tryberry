# Ранбук: ротация секретов после инцидента 2026-07-03

Чек-лист верхнего уровня — `docs/SECURITY-HARDENING.md` §4. Здесь — порядок,
команды и проверки. Все секреты считаем засвеченными (сервер был открыт наружу,
была RCE-попытка).

**Общие правила:**

- Работать из чистого шелла (никаких `set -a; . ./.env` — экспортированные
  переменные перебивают `.env` при интерполяции compose; `make deploy` это
  проверяет через `guard-clean-shell`).
- Генерация локальных секретов: `openssl rand -base64 24 | tr -d '/+='`.
- После правки `.env` контейнеры сами ничего не подхватывают — нужен
  `up -d --force-recreate <сервисы>` (алиас ниже).
- Один секрет за раз → проверка → следующий. Так окно даунтайма на каждый
  канал — минуты.

```bash
cd ~/projects/tryberrybot
alias dcp='docker compose -f docker-compose.yml -f docker-compose.prod.yml'
```

Кто что потребляет (из compose):

| Секрет | Сервисы |
|---|---|
| `TELEGRAM_BOT_TOKEN` | api, bot-worker, notifier, scraper |
| `TELEGRAM_ALERT_BOT_TOKEN` | alertmanager |
| `VK_GROUP_TOKEN` | bot-worker, notifier |
| `VK_CALLBACK_SECRET`, `VK_CONFIRMATION` | api |
| `MAX_BOT_TOKEN` | api, bot-worker, notifier |
| `MAX_CALLBACK_SECRET`, `MAX_WEBHOOK_URL` | api |
| `TELEGRAM_WEBHOOK_SECRET` | api (в polling-режиме не используется) |
| `ROBOKASSA_PASSWORD1/2` | bot-worker / api+bot-worker |
| `YOOKASSA_SECRET_KEY` | api, bot-worker (ЮKassa за флагом) |
| `S3_ACCESS_KEY_ID/SECRET` | backup-s3-sync (rclone) |
| `GRAFANA_ADMIN_PASSWORD` | grafana (hash уже в grafana_data!) |

## Порядок (от безрискового к чувствительному)

### 1. Grafana (2 мин, без даунтайма)
1. Сгенерить пароль, вписать в `.env` → `GRAFANA_ADMIN_PASSWORD`.
2. **И сменить в самой Grafana** (профиль admin → Change password) — env
   влияет только на первый запуск, hash живёт в `grafana_data`.
3. Проверка: логин на https://grafana.tryberry.ru новым паролем.

### 2. TELEGRAM_ALERT_BOT_TOKEN (алерт-бот)
1. BotFather → выбрать алерт-бота → `/revoke` (старый токен умирает сразу).
2. Новый токен в `.env` → `dcp up -d --force-recreate alertmanager`.
3. Проверка: `docker exec pt_alertmanager amtool alert add test_alert
   severity=warning --alertmanager.url=http://localhost:9093` → сообщение
   в алерт-чате пришло → `amtool silence`/само погаснет.

### 3. TELEGRAM_BOT_TOKEN (основной бот, даунтайм ~1–2 мин)
1. BotFather → основной бот → `/revoke`.
2. Токен в `.env` → `dcp up -d --force-recreate api bot-worker notifier scraper`.
3. Проверка: написать боту `/start` или `/myplan`; в логах bot-worker нет 401.

### 4. VK_GROUP_TOKEN + VK_CALLBACK_SECRET (вместе, один экран ЛК)
1. Сгенерить новый `VK_CALLBACK_SECRET` заранее.
2. ЛК сообщества → Настройки → Работа с API: перевыпустить ключ доступа
   (токен) и вписать новый «Секретный ключ» в Callback API.
3. Оба значения в `.env` → `dcp up -d --force-recreate api bot-worker notifier`.
4. Проверка: в ЛК VK Callback-сервер «подтверждён» (`VK_CONFIRMATION` не
   меняется); написать VK-боту. VK ретраит недоставленные события — короткое
   окно не теряет сообщения.

### 5. MAX_BOT_TOKEN + MAX_CALLBACK_SECRET
1. Перевыпустить токен MAX-бота (@MasterBot в MAX), новый секрет сгенерить.
2. В `.env` → `dcp up -d --force-recreate api bot-worker notifier`.
3. Проверка: убедиться, что webhook-подписка перерегистрировалась (лог api
   при старте) и бот в MAX отвечает.

### 6. TELEGRAM_WEBHOOK_SECRET
Бот в polling (`WEBHOOK_ENABLED=false`), секрет не используется, но переменная
должна быть непустой (WARN при `up`). Просто сгенерить и вписать; подхватится
при ближайшем пересоздании api (можно вместе с шагом 7).

### 7. ROBOKASSA_PASSWORD1 / ROBOKASSA_PASSWORD2
1. ЛК Робокассы → настройки магазина → сменить оба пароля. Применяются сразу,
   поэтому: сменил в ЛК → тут же в `.env` →
   `dcp up -d --force-recreate api bot-worker`.
2. Окно в пару минут безопасно: result-колбэки Робокасса ретраит, подпись
   сойдётся после рестарта.
3. Проверка: тестовый платёж/продление подписки, в логах api подпись валидна.

### 8. YOOKASSA_SECRET_KEY
ЮKassa за флагом (выключена) — перевыпустить ключ в ЛК, обновить `.env`.
Рестарт тот же, что в шаге 7 (можно объединить).

### 9. S3-ключи Selectel (бэкапы)
1. ЛК Selectel → IAM/сервисные пользователи → создать НОВЫЙ ключ (старый пока
   не удалять).
2. `.env` → `S3_ACCESS_KEY_ID`/`S3_SECRET_ACCESS_KEY` →
   `dcp up -d --force-recreate backup-s3-sync`.
3. Проверка: `dcp logs --since 5m backup-s3-sync` — sync без ошибок 403;
   затем удалить старый ключ в ЛК.

### 10. xray / wireguard (по возможности)
- VLESS un1.pro: UUID общий для подписки — перевыпуск = запросить новую
  подписку у провайдера; после — пересобрать `xray/config.json` из свежего
  `.example` и `dcp up -d --force-recreate xray`.
- WG-резерв: перегенерить пару ключей (`wg genkey`), обновить у провайдера
  туннеля и в `wireguard/wg_confs/wg0.conf`; затем
  `dcp up -d --force-recreate wg-proxy && dcp up -d --force-recreate tinyproxy`
  (tinyproxy обязательно после wg-proxy — общий netns).
- Если провайдер перевыпуск не даёт — зафиксировать как принятый риск
  (наружу эти ключи дают только egress-туннель, не вход на сервер).

## Финал

```bash
chmod 600 .env
make security-check
```

- Отметить пункты в `docs/SECURITY-HARDENING.md` §4, закоммитить.
- Закрыть задачу ротации в Google Tasks.
- После ротации: замаскировать Redis-пароль в логах token-miner
  (пишет `redis://:<пароль>@...` в INFO при старте) — мелочь, но незачем.
