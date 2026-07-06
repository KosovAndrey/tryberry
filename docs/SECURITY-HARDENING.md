# Хардинг сервера — чек-лист

Документ-план по безопасности прод-VPS. Составлен после **инцидента 2026-07-03**
(порты Redis/Postgres/Kafka торчали в интернет → Redis заслейвили rogue-replica
атакой, в Postgres завели бэкдор-суперюзер `pgg_superadmins`). Подробности инцидента
— в памяти `security-incident-2026-07-03`.

Идёшь сверху вниз. Отмечай `[x]` по мере выполнения. Приоритеты: **P0** (сделано в
ходе инцидента, проверить), **P1** (сделать в первую очередь), **P2** (в ближайшее
время), **P3** (желательно).

> **Честная оговорка.** «Невзламываемых» серверов не бывает — цель хардинга в том,
> чтобы (1) свести поверхность атаки к минимуму (nginx 443/80 + SSH по ключу),
> (2) каждый следующий слой стоил атакующему отдельного 0-day (defense-in-depth:
> фаервол → закрытые порты → пароли → непривилегированные роли), и (3) любое
> отклонение орало в Telegram в течение минут (Prometheus + cron-self-check),
> а бэкапы позволяли восстановиться. После выполнения этого списка повторение
> сценария 2026-07-03 требует уже не «нашёл открытый порт», а цепочку эксплойтов.

---

## Статус: что уже в git (ветка security/server-hardening)

Готово в коде/конфигах, **на сервере применить по раннбуку ниже**:

- `docker-compose.yml`: Redis `--requirepass` + отключены `FLUSHALL/FLUSHDB/DEBUG`
  (вдобавок к `REPLICAOF/SLAVEOF/MODULE`); все `REDIS_URL` с паролем.
- `docker-compose.yml`: приложение ходит в PG ролью **`tryberry_app`** (только DML),
  postgres-exporter — ролью **`tryberry_monitor`** (только pg_monitor). Суперюзер
  `user` остаётся только для миграций и pg_dump.
- `docker-compose.yml`: Grafana safe-by-default — пароль из `.env` обязателен,
  анонимный доступ выключен даже без прод-оверрайда.
- `Makefile`: `make deploy` = оба `-f` + **guard от «грязного шелла»** (отказ, если
  в шелле экспортированы DATABASE_URL и др.) + **авто-проверка портов** после
  деплоя (`make check-ports`).
- `scripts/pg-create-roles.sh` — создание/обновление ролей PG (идемпотентен).
- `scripts/server-harden.sh` — UFW + iptables DOCKER-USER + SSH-хардинг +
  fail2ban + unattended-upgrades + chmod .env (одноразовый, идемпотентен).
- `scripts/security-selfcheck.sh` — cron-самопроверка (порты/Redis/PG-роли/фаервол)
  с алертом в Telegram; также `make security-check`.
- `monitoring/prometheus/rules/security.yml` + `monitoring/postgres-exporter/queries.yaml`
  — алерты: Redis стал репликой / у Redis появились реплики / в PG лишний
  суперюзер или login-роль.

---

## Раннбук: порядок применения на сервере

Строго по порядку — сначала роли и секреты, потом рестарт стека.

```bash
cd ~/projects/tryberrybot
git fetch && git checkout main && git pull   # после мержа ветки

# 1. Новые секреты в .env (генерация: openssl rand -base64 24 | tr -d '/+=')
#    ДОПИСАТЬ строки (без пробелов вокруг =):
#      REDIS_PASSWORD=<...>
#      APP_DB_PASSWORD=<...>
#      MONITOR_DB_PASSWORD=<...>
#    GRAFANA_ADMIN_PASSWORD уже должен быть; если нет — добавить.
chmod 600 .env

# 2. Роли Postgres (пока стек ещё работает на старом конфиге)
./scripts/pg-create-roles.sh

# 3. Перезапуск стека на новых кредах (ЧИСТЫЙ шелл, без set -a; . ./.env!)
make deploy
#    Redis пересоздастся с requirepass, сервисы — с новыми REDIS_URL/DATABASE_URL.
#    В конце deploy сам прогонит check-ports.

# 4. Проверить, что всё поднялось
docker ps --format '{{.Names}}\t{{.Status}}' | sort
docker logs pt_bot_worker --since 5m 2>&1 | grep -iE 'error|fail' | head
make security-check   # (п.4 фаервола упадёт до шага 5 — это ожидаемо)

# 5. Хост: фаервол/SSH/fail2ban/авто-патчи
#    СНАЧАЛА убедись, что заходишь по ключу: ssh -o PasswordAuthentication=no <host>
sudo ./scripts/server-harden.sh

# 6. Cron-самопроверка раз в 15 минут
sudo tee /etc/cron.d/tryberry-selfcheck >/dev/null <<'CRON'
*/15 * * * * root /home/kosovandrey/projects/tryberrybot/scripts/security-selfcheck.sh >> /var/log/tryberry-selfcheck.log 2>&1
CRON

# 7. Снаружи (с другой машины): открыты только 22/80/443
nmap -Pn <IP>
```

После этого пройтись по чек-листу ниже и расставить `[x]`.

---

## P0 — уже сделано в ходе инцидента (проверено 2026-07-04)

- [x] **Порты закрыты.** `docker ps` → наружу только nginx 80/443.
- [x] **Redis:** `role:master`, `REPLICAOF` → `unknown command`.
- [x] **Postgres:** бэкдор `pgg_superadmins` удалён (`\du` → только `user`),
      пароль сменён, все креды через `${POSTGRES_PASSWORD}` из `.env`.

---

## P1 — сделать в первую очередь

### 1. Деплой-гигиена (корневая причина инцидента)
- [x] `make deploy` = оба `-f` (`COMPOSE_PROD` в Makefile) — было и раньше.
- [x] Guard от «грязного шелла»: `make deploy` отказывается работать, если в шелле
      экспортированы `DATABASE_URL`/`REDIS_URL`/пароли (перебивают `${...}` из
      `.env` — ловили дважды: OZON_PROXY_URL и DATABASE_URL).
- [x] Авто-проверка портов после каждого деплоя (`make check-ports` в конце deploy).
- [ ] **Правило для себя: деплоить ТОЛЬКО через `make deploy` / `make deploy-api`.**
      Руками `docker compose up` не набирать вообще.

### 2. Хост-фаервол (UFW + DOCKER-USER)
- [ ] `sudo ./scripts/server-harden.sh` — ставит всё разом:
      - UFW: default deny incoming, открыты 22/80/443;
      - ⚠️ **Docker обходит UFW**, поэтому отдельно цепочка `DOCKER-USER`:
        DROP всего входящего к контейнерам с внешнего интерфейса, кроме 80/443.
        Даже если compose снова опубликует порт (регресс инцидента) — снаружи
        он будет закрыт. Персистентность — через `/etc/ufw/after.rules` (ufw
        применяет при старте/reload). ⚠️ НЕ ставить `iptables-persistent`: он
        конфликтует с ufw, apt при установке молча удаляет ufw (наступили
        2026-07-04 — selfcheck поймал пропажу UFW).
- [ ] Проверить снаружи с другой машины: `nmap -Pn <IP>` → только 22/80/443.

### 3. SSH
- [ ] Входит в `server-harden.sh`: `PasswordAuthentication no`,
      `PermitRootLogin no`, `MaxAuthTries 4` (drop-in `sshd_config.d/99-hardening.conf`,
      `sshd -t` перед рестартом). **Сначала проверь вход по ключу!**
- [ ] fail2ban с jail `sshd` (бан по нарастающей до недели) — там же.
- [ ] (опц.) сменить SSH-порт с 22 на нестандартный — снижает шум сканеров.

### 4. Ротация секретов (сервер был доступен извне)
- [x] Раз БД и Redis торчали наружу, а RCE-попытка была — считать все секреты
      потенциально засвеченными. Сменить в `.env` и у провайдеров (ротация 2026-07-06):
      - [x] `TELEGRAM_BOT_TOKEN` (BotFather → /revoke) + `TELEGRAM_ALERT_BOT_TOKEN`
      - [x] `VK_GROUP_TOKEN` (настройки сообщества → перевыпустить)
      - [x] `MAX_BOT_TOKEN`
      - [x] `ROBOKASSA_PASSWORD1/2` (ЛК Робокассы). `YOOKASSA_SECRET_KEY` — n/a:
            провайдер `PAYMENT_PROVIDER=robokassa`, ЮKassa не активна, ключ убран из `.env`
      - [x] `S3_ACCESS_KEY_ID`/`S3_SECRET_ACCESS_KEY` (ЛК Selectel; создан новый ключ,
            sync 20/20 без 403 — старый ключ удалить в ЛК после подтверждения)
      - [ ] `GRAFANA_ADMIN_PASSWORD` — отложено как принятый риск: Grafana отдаёт только
            дашборды (datasource — read-only monitor-роль), доступ к VPS закрыт. Сменить
            при желании (2 мин): env + профиль admin → Change password (hash в grafana_data)
      - [x] `POSTGRES_PASSWORD` (сменён в ходе инцидента)
      - [x] `VK_CALLBACK_SECRET`, `MAX_CALLBACK_SECRET`
      - [x] `TELEGRAM_WEBHOOK_SECRET` — n/a: в polling-режиме не используется и
            в `.env` не задан, ротировать нечего (см. ранбук, шаг 6)
      - [x] `xray/config.json` — старые un1.pro выведены, новый сервер der9.joybang.site
            (2 плеча: reality+vision tcp:443 + ws+tls:9443), egress на Telegram работает.
            `wireguard/*` — резервное плечо, egress-only, оставлено как принятый риск
- [x] `chmod 600 .env` (входит в `server-harden.sh`).
- [x] `.env`, `xray/config.json`, `wireguard/*` в `.gitignore` (секреты не в git).

---

## P2 — в ближайшее время

### 5. Postgres
- [x] **Не-суперюзер роль** `tryberry_app` (только DML на public) — в compose и
      `scripts/pg-create-roles.sh`. Применить на сервере (раннбук, шаги 1–3).
- [x] Отдельная роль `tryberry_monitor` (pg_monitor) для postgres-exporter —
      DSN экспортера больше не содержит админ-пароль.
- [ ] (опц.) Ужесточить `pg_hba.conf`: доступ только из docker-подсети,
      `scram-sha-256`. Порт и так не опубликован — низкий приоритет.

### 6. Redis
- [x] `--requirepass "${REDIS_PASSWORD}"` + `REDIS_URL` с паролем во всех сервисах
      + `REDISCLI_AUTH` для healthcheck + пароль у redis-exporter. Применить на
      сервере (раннбук, шаги 1, 3).
- [x] Дополнительно отключены `FLUSHALL/FLUSHDB/DEBUG` (код их не использует;
      `CONFIG` оставлен — нужен redis-exporter'у).

### 7. Grafana
- [x] Safe-by-default в базовом compose: пароль обязателен из `.env`, анонимный
      доступ выключен. Регресс «деплой без оверрайда = admin/admin» невозможен.
- [ ] Если старый пароль был `admin` и им логинились — сменить и в UI (профиль →
      Change password): hash хранится в volume grafana_data.

### 8. Обновления и патчи
- [ ] `unattended-upgrades` — входит в `server-harden.sh`.
- [ ] Регулярно обновлять docker-образы (особенно redis/postgres/nginx) — pinned
      версии обновлять осознанно. Ритм: раз в месяц `docker compose pull` +
      redeploy в окно.

---

## P3 — желательно / мониторинг

- [x] **Алерты на признаки компрометации** (`monitoring/prometheus/rules/security.yml`):
      - `RedisHijackedAsReplica` — role != master (ровно наш инцидент);
      - `RedisUnexpectedSlaves` — кто-то реплицируется с нас;
      - `PostgresUnexpectedSuperuser` / `PostgresUnexpectedLoginRole` — лишние
        роли (метрики из `monitoring/postgres-exporter/queries.yaml`).
- [x] **Периодический self-check** (`scripts/security-selfcheck.sh` + cron):
      порты, Redis-роль, PG-роли, UFW/DOCKER-USER → алерт в Telegram. Работает
      независимо от Prometheus-стека (если мониторинг положили — cron останется).
- [x] **Бэкапы:** проверено 2026-07-04 — дампы кладутся локально (`backups/daily`,
      ротация 7d/4w/6m) и синкаются в S3 (бакет `tryberry-db-backups`, та же
      ротация + `last/`).
- [x] **Restore протестирован** 2026-07-04: свежий дамп → база `restore_test` в
      том же контейнере, ноль ошибок; users/subscriptions/payments совпали 1:1,
      products/price_history меньше на дневной прирост (дамп ночной — норма).
      Рецепт: `docker exec pt_postgres psql -U user -d postgres -c "CREATE DATABASE restore_test;"`
      → `zcat backups/daily/tryberrybot-latest.sql.gz | docker exec -i pt_postgres psql -U user -d restore_test -q`
      → сверить count(*) → `DROP DATABASE restore_test;`. Повторять хотя бы раз
      в квартал / после крупных миграций.
- [ ] **docker.sock у promtail** смонтирован read-only — это осознанный риск
      (нужен для сбора логов). Не добавлять socket другим контейнерам. При
      желании убрать совсем — перейти на журнал file-driver.
- [ ] Ревизия capabilities: `cap_add: NET_ADMIN/SYS_MODULE` только у wg-proxy
      (нужно для туннеля). Новым сервисам capabilities не давать.
- [ ] Kafka без аутентификации (PLAINTEXT) — допустимо только потому, что порт
      не опубликован и docker-сеть изолирована. Если когда-нибудь понадобится
      внешний доступ — только SASL/TLS, не публикация порта.
- [ ] (опц.) Зеркалить `/var/log/auth.log` хоста в Loki (promtail уже есть) —
      история SSH-попыток переживёт компрометацию хоста.
- [ ] (опц.) Уведомление о логине в SSH в Telegram (pam_exec + curl) — мгновенно
      видно чужой вход.

---

## Быстрая проверка «всё ли закрыто» (после хардинга)

```bash
make security-check            # всё разом (то же гоняет cron каждые 15 мин)

# или руками:
docker ps --format '{{.Names}}: {{.Ports}}' | tr ',' '\n' | grep -E '0\.0\.0\.0|\[::\]'   # только nginx 80/443
docker exec pt_redis redis-cli role | head -1                                              # NOAUTH? → есть пароль; с auth: master
docker exec pt_postgres psql -U user -d tryberrybot -c "\du"                               # user + tryberry_app + tryberry_monitor
sudo ufw status verbose
sudo iptables -L DOCKER-USER -n
```
