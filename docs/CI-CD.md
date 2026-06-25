# CI/CD — GitLab + self-hosted раннер на VPS

Пайплайн (`.gitlab-ci.yml`):
- **test** — на каждый push/MR: `go build ./... && go test ./...` в контейнере `golang:1.26`.
- **deploy** — вручную (кнопкой), только `main`: тянет `main` в рабочий каталог на
  VPS и делает `make deploy` (пересборка `api` + воркеров с `GIT_COMMIT`, `up -d`,
  reload nginx).

Раннер стоит на **том же VPS**, где прод. Поэтому деплой — обычный локальный
`docker compose up`, без SSH и без выноса секретов в CI: `.env`, `xray/config.json`,
`wireguard/*`, `nginx/.htpasswd` остаются на сервере (в `.gitignore`).

## Одноразовая настройка раннера

Делается на VPS под пользователем, который владеет каталогом проекта и состоит в
группе `docker` (тот же, под кем ты обычно запускаешь `docker compose`). Ниже —
`kosovandrey`.

### 1. Установить gitlab-runner

```bash
curl -L "https://packages.gitlab.com/install/repositories/runner/gitlab-runner/script.deb.sh" | sudo bash
sudo apt-get install -y gitlab-runner
```

### 2. Перенастроить сервис на запуск ОТ нужного пользователя

По умолчанию раннер работает от `gitlab-runner` — ему не хватит прав на каталог
проекта и (возможно) на docker. Перевешиваем на `kosovandrey`:

```bash
# убедись, что пользователь в группе docker
sudo usermod -aG docker kosovandrey

sudo gitlab-runner uninstall
sudo gitlab-runner install --user kosovandrey --working-directory /home/kosovandrey/gitlab-runner
sudo gitlab-runner start
```

### 3. Зарегистрировать раннер (shell executor, тег `tryberry`)

Токен: GitLab → проект → **Settings → CI/CD → Runners → New project runner**
(или возьми registration token там же).

```bash
sudo gitlab-runner register \
  --non-interactive \
  --url "https://gitlab.com/" \
  --token "<RUNNER_AUTH_TOKEN>" \
  --executor "shell" \
  --description "tryberry-vps"
```

В UI раннера проставь тег **`tryberry`** (он указан в `tags:` обоих джоб). Если
регистрировал старым registration-token'ом — добавь `--tag-list "tryberry"`.

### 4. Проверить путь деплоя

`DEPLOY_DIR` в `.gitlab-ci.yml` = `/home/kosovandrey/projects/tryberrybot`. Если путь
другой — переопредели в **Settings → CI/CD → Variables** (`DEPLOY_DIR`), не трогая файл.

## Как пользоваться

- Пушишь ветку / открываешь MR → автоматически гоняется **test**. Красный — не мержим.
- Мержишь в `main` → в пайплайне `main` появляется джоба **deploy** со статусом
  manual → жмёшь ▶️ когда готов выкатить. Деплой идёт на прод.

Можно сделать деплой автоматическим (убрать `when: manual`), но пока выкатываем
кнопкой — без сюрпризов.

## Что НЕ делает пайплайн (намеренно)

- **Миграции БД не применяются автоматически.** У нас нет goose-трекинга на проде
  (миграции подаём Up-блоком в psql вручную, см. tryberrybot-HANDOFF.md). Авто-
  применение всех миграций каждый деплой было бы опасно. Новую миграцию накатывай
  руками ПЕРЕД деплоем кода, который её требует.
- **Секреты не трогает** — берёт готовые из рабочего каталога на VPS.

## Безопасность

Shell-executor исполняет скрипты джоб прямо на VPS от `kosovandrey` (с доступом к
docker). Для приватного соло-репозитория это ок. Важно:
- держать ветку `main` **protected**, деплой — только с неё;
- осторожно с MR от форков/чужих — `.gitlab-ci.yml` берётся из ветки MR, т.е. чужой
  код мог бы выполниться в `test` на хосте. Для соло-репо неактуально; если появятся
  контрибьюторы — включи «pipelines for fork MRs» только после ревью или вынеси
  test в docker executor.

## Локальные команды (Makefile)

На VPS то же самое руками:
```bash
make ci-test       # сборка+тесты в golang-контейнере
make deploy-api    # пересобрать+перезапустить только api (с GIT_COMMIT)
make deploy        # пересобрать api+воркеры, up -d, reload nginx
make nginx-reload  # nginx -t && reload
```
