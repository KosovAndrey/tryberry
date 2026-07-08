# Production deploy — пошаговая инструкция

Развёртывание `tryberry.ru` с nginx, Let's Encrypt и закрытой Grafana.

Прод-сервер: `ssh kosovandrey@194.164.245.150`, проект в `~/projects/tryberrybot`.

---

## Обычный деплой (рутинный, после мержа в main)

Полный цикл — всегда с ОБОИМИ `-f` (без prod-оверлея наружу торчат порты
Redis/PG/Kafka — инцидент 2026-07-03):

```bash
ssh kosovandrey@194.164.245.150
cd ~/projects/tryberrybot
git pull

# при необходимости: новые env-переменные (смотри диф docker-compose.yml)
nano .env

# пересобрать и перекатить ТОЛЬКО затронутые сервисы (пример)
docker compose -f docker-compose.yml -f docker-compose.prod.yml build scraper bot-worker search-worker
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d scraper bot-worker search-worker

# проверка
docker compose -f docker-compose.yml -f docker-compose.prod.yml ps
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs --tail 50 scraper bot-worker
```

Замечания:

- Реплики зашиты в prod-оверлей (`deploy.replicas`, сейчас scraper=3) —
  отдельный `--scale` не нужен, `up -d` сам держит нужное число.
- Миграции goose на проде — руками через psql от суперюзера (PG-порт закрыт,
  ghcr-goose недоступен): `docker compose -f docker-compose.yml -f docker-compose.prod.yml exec postgres psql -U postgres -d tryberrybot`.
- Быстрая проверка метрик без Grafana:
  `docker compose -f docker-compose.yml -f docker-compose.prod.yml exec prometheus wget -qO- 'http://localhost:9090/api/v1/query?query=<PromQL>'`.

Дальше — историческая пошаговая инструкция ПЕРВИЧНОЙ настройки (nginx/TLS/htpasswd).

---

## Структура файлов

После заливки в репозиторий должно получиться так:

```
tryberrybot/
├── docker-compose.yml                          # ← существующий, не трогаем
├── docker-compose.prod.yml                     # NEW
├── .env                                        # ← существующий, добавим строки
├── .env.prod.example                           # NEW (справочно)
├── nginx/
│   ├── nginx.conf                              # NEW
│   ├── conf.d/
│   │   └── tryberry.conf                       # NEW
│   ├── html/
│   │   └── index.html                          # NEW (лендинг)
│   ├── .htpasswd                               # ← уже есть на VM, в репо НЕ кладём
│   └── certbot/                                # ← создаётся certbot'ом, в .gitignore
│       ├── conf/
│       └── www/
├── scripts/                                    # NEW
│   └── init-letsencrypt.sh
└── monitoring/
    └── alertmanager/
        └── templates/
            └── telegram.tmpl                   # ← заменяем существующий
```

Добавь в `.gitignore`:

```
nginx/.htpasswd
nginx/certbot/
```

---

## Шаг 1. Подготовка VM

```bash
# Зайди по SSH
ssh kosovandrey@194.164.245.150

# Проверь docker compose plugin (нужна v2.20+ для !reset)
docker compose version

# Если нет — поставь
sudo apt update
sudo apt install -y docker-compose-plugin apache2-utils ufw curl
```

Если `docker compose version` показывает `< 2.20.0`, отдельные `ports:` в `docker-compose.yml` придётся убирать вручную (не через `!reset`). Скажи мне версию — переделаю.

---

## Шаг 2. Залить файлы

На рабочей машине (не на VM):

```bash
cd /path/to/local/tryberrybot
# распакуй файлы из /mnt/user-data/outputs/ в соответствующие места репо
# затем
git add nginx/ scripts/ docker-compose.prod.yml .env.prod.example \
        monitoring/alertmanager/templates/telegram.tmpl .gitignore
git commit -m "feat(deploy): nginx + certbot reverse proxy with landing"
git push
```

На VM:

```bash
cd ~/projects/tryberrybot
git pull
```

---

## Шаг 3. Заполнить .env

Открой `.env` на VM (`nano .env`) и допиши/обнови:

```bash
WEBHOOK_ENABLED=true
TELEGRAM_WEBHOOK_URL=https://tryberry.ru/webhook
CERTBOT_EMAIL=твоя.почта@example.com
GRAFANA_ADMIN_PASSWORD=$(сгенерировать ниже)
```

Сгенерировать надёжный пароль для Grafana:

```bash
openssl rand -base64 24
```

Скопируй вывод в `GRAFANA_ADMIN_PASSWORD=...` в `.env`.

---

## Шаг 4. Проверить/обновить htpasswd

У тебя на VM уже есть `nginx/.htpasswd` с одним пользователем. Если ты помнишь пароль — пропускай шаг.

Если нужно пересоздать (забыл пароль, хочешь сменить логин):

```bash
htpasswd -c nginx/.htpasswd admin
# спросит пароль, введи дважды; флаг -c перезаписывает файл
```

Этот пароль будет запрашиваться **до** того как тебя пустит к Grafana.

⚠️ **`nginx/.htpasswd` НЕ должен попадать в git** — добавь в `.gitignore`.

---

## Шаг 5. Первый запуск certbot

В корне проекта на VM:

```bash
# Загружаем .env в shell, чтобы скрипт увидел CERTBOT_EMAIL
set -a; source .env; set +a

# Сначала пробуем staging (тестовые сертификаты LE, не валидные для браузеров,
# но и не тратят rate-limit — у LE 5 попыток в неделю на домен)
STAGING=1 bash scripts/init-letsencrypt.sh
```

Если staging прошёл успешно — повтори без `STAGING`:

```bash
bash scripts/init-letsencrypt.sh
# скрипт спросит "Перезаписать?" — отвечай y
```

Должен увидеть в конце:

```
✅ Готово! Проверь:
    curl -I https://tryberry.ru
    curl -I https://grafana.tryberry.ru
```

---

## Шаг 6. Запустить всё остальное

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d
```

Проверь что всё поднялось:

```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml ps
```

Все сервисы должны быть `Up` или `Up (healthy)`.

---

## Шаг 7. Поставить Telegram webhook

```bash
# Загружаем токен из .env
set -a; source .env; set +a

# Регистрируем webhook у Telegram
curl -X POST "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/setWebhook" \
     -H "Content-Type: application/json" \
     -d '{"url":"https://tryberry.ru/webhook","drop_pending_updates":true}'

# Проверка
curl "https://api.telegram.org/bot${TELEGRAM_BOT_TOKEN}/getWebhookInfo"
```

В ответе должно быть `"url":"https://tryberry.ru/webhook"` и `"pending_update_count":0`.

---

## Шаг 8. Проверка

```bash
# Лендинг
curl -I https://tryberry.ru
# → 200 OK, server: nginx

# Webhook (Telegram пишет только POST; GET должен дать 405)
curl -I https://tryberry.ru/webhook
# → 405 Method Not Allowed

# Grafana без авторизации
curl -I https://grafana.tryberry.ru
# → 401 Unauthorized (это правильно)

# Grafana с basic auth
curl -I -u kosov:твой_basic_auth_password https://grafana.tryberry.ru
# → 302 редирект на /login (это Grafana дальше требует свой логин)
```

В браузере — открой `https://grafana.tryberry.ru`:
1. Сначала спросит `kosov` / basic auth password (из шага 4)
2. Потом Grafana попросит `admin` / `GRAFANA_ADMIN_PASSWORD` (из шага 3)

Напиши что-нибудь боту в Telegram — должно прийти меню.

---

## Шаг 9. Закрыть ВМ firewall'ом

После того как всё работает:

```bash
sudo ufw default deny incoming
sudo ufw default allow outgoing
sudo ufw allow 22/tcp     # ВАЖНО: сначала SSH! Иначе отрежешь сам себя
sudo ufw allow 80/tcp
sudo ufw allow 443/tcp
sudo ufw status verbose   # проверь правила
sudo ufw enable           # только теперь включай
```

После этого `docker compose ps` останется работать (docker не использует системный firewall на published ports), а внешние подключения на 9090/3000/etc будут отрезаны.

---

## Troubleshooting

**`certbot: rate limit exceeded`**  
Let's Encrypt разрешает 5 неудачных попыток на домен в час и 50 валидных сертификатов в неделю. Если упёрся — жди час или используй `STAGING=1`.

**`docker compose: services.api: !reset is not supported`**  
У тебя старый compose plugin. Либо обнови (`sudo apt install docker-compose-plugin` после `apt update`), либо вручную убери `ports:` у grafana/prometheus/etc из основного `docker-compose.yml`.

**Лендинг открывается, а `/webhook` отдаёт 502**  
API контейнер не стартанул или nginx не видит его в docker-сети. Проверь:
```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml logs api
docker compose -f docker-compose.yml -f docker-compose.prod.yml exec nginx ping api
```

**Grafana показывает «Invalid origin» или CSRF ошибки**  
Проверь что в compose стоит `GF_SERVER_ROOT_URL=https://grafana.tryberry.ru` (это в `docker-compose.prod.yml`).

**В Telegram приходит алерт со ссылкой на `localhost:3000`**  
Не подменился `telegram.tmpl`. Сделай:
```bash
docker compose -f docker-compose.yml -f docker-compose.prod.yml restart alertmanager
```

---

## Что дальше

- Поставить `fail2ban` на nginx — банить IP за множественные 401 на grafana
- Настроить бэкап `/var/lib/docker/volumes/*_postgres-data` (хотя бы `rsync` раз в день)
- Подключить Yandex Object Storage для долгого хранения логов из Loki
