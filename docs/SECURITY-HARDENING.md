# Хардинг сервера — чек-лист

Документ-план по безопасности прод-VPS. Составлен после **инцидента 2026-07-03**
(порты Redis/Postgres/Kafka торчали в интернет → Redis заслейвили rogue-replica
атакой, в Postgres завели бэкдор-суперюзер `pgg_superadmins`). Подробности инцидента
— в памяти `security-incident-2026-07-03`.

Идёшь сверху вниз. Отмечай `[x]` по мере выполнения. Приоритеты: **P0** (сделано в
ходе инцидента, проверить), **P1** (сделать в первую очередь), **P2** (в ближайшее
время), **P3** (желательно).

---

## P0 — уже сделано в ходе инцидента (проверить, что живо)

- [ ] **Порты закрыты.** `docker port pt_redis; docker port pt_postgres; docker port pt_kafka` → пусто.
      В базовом `docker-compose.yml` все published-порты привязаны к `127.0.0.1`;
      прод-оверрайд `!reset []` убирает их совсем. Публичен только nginx 80/443.
- [ ] **Redis:** `role:master`, `--rename-command REPLICAOF/SLAVEOF/MODULE ""`
      (проверка: `docker exec pt_redis redis-cli REPLICAOF NO ONE` → `unknown command`).
- [ ] **Postgres:** бэкдор `pgg_superadmins` удалён (`\du` → только `user`),
      пароль сменён, все креды через `${POSTGRES_PASSWORD}` из `.env`.

---

## P1 — сделать в первую очередь

### 1. Деплой-гигиена (корневая причина инцидента)
- [ ] Деплоить прод **всегда** с обоими файлами и **из чистого шелла**:
      ```bash
      docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
      ```
- [ ] Зашить это в скрипт/Makefile/alias (`make deploy` или `deploy.sh`), чтобы
      нельзя было забыть второй `-f`.
- [ ] **Не делать `set -a; . ./.env` в шелле, из которого запускаешь `docker compose`** —
      экспорт перебивает `${...}` из `.env` (ловили дважды: OZON_PROXY_URL и DATABASE_URL).
      Для probe-команд оборачивать в подшелл: `( set -a; . ./.env; docker run ... )`.
- [ ] После каждого деплоя проверять: `docker port pt_redis pt_postgres pt_kafka` = пусто.

### 2. Хост-фаервол (UFW)
- [ ] Оставить открытыми только **22 (SSH), 80, 443**. Всё остальное — закрыто.
      ```bash
      sudo ufw default deny incoming
      sudo ufw default allow outgoing
      sudo ufw allow 22/tcp
      sudo ufw allow 80/tcp
      sudo ufw allow 443/tcp
      sudo ufw enable
      ```
- [ ] ⚠️ **ВАЖНО: Docker обходит UFW.** Опубликованный порт (`ports:`) Docker
      добавляет в цепочку `DOCKER` в iptables мимо UFW — т.е. `ufw deny 6379` НЕ
      закроет опубликованный контейнером порт. Поэтому главная защита — **не
      публиковать порты наружу** (уже: loopback + `!reset`). Для гарантии можно
      добавить правило в `DOCKER-USER`:
      ```bash
      # запретить доступ к docker-портам со всех адресов, кроме локальных
      sudo iptables -I DOCKER-USER -i eth0 ! -s 127.0.0.1 -p tcp -m multiport --dports 6379,5432,5433,9092,9090,3000,16686 -j DROP
      ```
      (подставь реальный внешний интерфейс вместо `eth0`; сделай persistent через
      `netfilter-persistent save`.)
- [ ] Проверить снаружи, что порты закрыты: `nmap -Pn <IP>` с другой машины —
      должны отвечать только 22/80/443.

### 3. SSH
- [ ] Только ключи, пароли выключить: в `/etc/ssh/sshd_config`
      `PasswordAuthentication no`, `PermitRootLogin prohibit-password`,
      `PubkeyAuthentication yes`. `sudo systemctl restart ssh`.
      (Сначала убедись, что твой ключ работает!)
- [ ] Поставить **fail2ban** (бан брутфорса SSH): `sudo apt install fail2ban`,
      включить jail `sshd`.
- [ ] (опц.) сменить SSH-порт с 22 на нестандартный — снижает шум сканеров.

### 4. Ротация секретов (сервер был доступен извне)
- [ ] Раз БД и Redis торчали наружу, а RCE-попытка была — считать все секреты
      потенциально засвеченными. Сменить в `.env` и у провайдеров:
      `TELEGRAM_BOT_TOKEN`, `VK_*`, `MAX_*`, `YOOKASSA_*`, `ROBOKASSA_*`,
      `S3_ACCESS_KEY_ID`/`S3_SECRET_ACCESS_KEY`, `GRAFANA_ADMIN_PASSWORD`,
      `POSTGRES_PASSWORD` (уже сменён).
- [ ] Проверить права `.env`: `chmod 600 .env`, владелец — деплой-юзер.
- [ ] Убедиться, что `.env`, `xray/config.json`, `wireguard/*` в `.gitignore`
      (секреты не в git).

---

## P2 — в ближайшее время

### 5. Postgres
- [ ] Завести **не-суперюзер роль** для приложения (сейчас `user` = Superuser —
      при RCE через БД это даёт больше). Роль с правами только на схему `public`
      базы `tryberrybot`, без CREATEROLE/SUPERUSER. Обновить `POSTGRES_PASSWORD`/
      DATABASE_URL под неё. Оставить `user`-суперюзера только для миграций/админки.
- [ ] Ужесточить `pg_hba.conf`: доступ только из docker-сети, `scram-sha-256`.

### 6. Redis
- [ ] Включить `--requirepass "${REDIS_PASSWORD}"` (secret в `.env`) и обновить
      `REDIS_URL=redis://:pass@redis:6379` во всех сервисах. Defense-in-depth
      поверх закрытого порта.

### 7. Grafana
- [ ] Сменить админ-пароль (при деплое без оверрайда была `admin/admin` на
      открытом `:3000`). Проверить `GRAFANA_ADMIN_PASSWORD` в `.env`, анонимный
      доступ выключен (прод-оверрайд уже ставит).

### 8. Обновления и патчи
- [ ] `unattended-upgrades` для авто-патчей безопасности ОС.
- [ ] Регулярно обновлять docker-образы (особенно redis/postgres/nginx) — pinned
      версии обновлять осознанно.

---

## P3 — желательно / мониторинг

- [ ] **Алерты на признаки компрометации** (Prometheus/Alertmanager уже есть):
      - Redis `role != master` (заслейвили) — по `redis_instance_info`/exporter;
      - неожиданный рост числа ролей в Postgres или появление суперюзера ≠ `user`;
      - процессы с аномальным CPU (майнер) через node-exporter.
- [ ] **Периодический self-check** (cron): скрипт проверяет `docker port` пусто,
      Redis `role:master`, в Postgres нет ролей кроме ожидаемых — алертит в
      Telegram/лог при отклонении.
- [ ] **Бэкапы:** проверить, что `postgres-backup` + `backup-s3-sync` реально
      кладут дампы (локально и в S3), и **протестировать restore** на пустой БД.
- [ ] Не монтировать docker socket в контейнеры без крайней нужды; ревизия
      capabilities контейнеров.
- [ ] (опц.) Зеркалить логи авторизации/аудита хоста в Loki для расследований.

---

## Быстрая проверка «всё ли закрыто» (после хардинга)

```bash
# порты наружу:
docker ps --format '{{.Names}}: {{.Ports}}' | grep -vE '127\.0\.0\.1|:80->|:443->' | grep -E '0\.0\.0\.0|:::' && echo "!!! есть открытые порты" || echo "ok: наружу только nginx"
# redis мастер + хардинг:
docker exec pt_redis redis-cli role | head -1
docker exec pt_redis redis-cli REPLICAOF NO ONE 2>&1 | grep -q unknown && echo "redis: REPLICAOF disabled ok"
# postgres роли:
docker exec pt_postgres psql -U user -d tryberrybot -c "\du"
# ufw:
sudo ufw status verbose
```
