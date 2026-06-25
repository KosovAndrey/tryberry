#!/usr/bin/env bash
# CI/CD без GitLab-пайплайнов (обход верификации GitLab, актуально для РФ).
# Запускается systemd-таймером раз в пару минут на ТОМ ЖЕ VPS, где прод:
#   1) git fetch; если origin/main не сдвинулся — выходим;
#   2) подтягиваем main;
#   3) make ci-test (сборка+тесты в golang-контейнере);
#   4) при успехе — make deploy (пересборка api+воркеров с GIT_COMMIT, up -d, reload nginx).
# Логи — в journald: journalctl -u tryberry-ci.service
set -euo pipefail

# Каталог репо = на уровень выше этого скрипта (scripts/..). Работает при любом пути.
REPO_DIR="$(cd "$(dirname "$0")/.." && pwd)"
cd "$REPO_DIR"

# Не наступаем на себя, если прошлый прогон (длинная сборка) ещё идёт.
exec 9>/tmp/tryberry-ci.lock
flock -n 9 || { echo "$(date -Is) другой прогон ещё идёт — пропускаю тик"; exit 0; }

git fetch --quiet origin main
local_sha="$(git rev-parse HEAD)"
remote_sha="$(git rev-parse origin/main)"
if [ "$local_sha" = "$remote_sha" ]; then
  exit 0   # ничего нового — тихо выходим
fi

echo "$(date -Is) main сдвинулся: ${local_sha:0:8} → ${remote_sha:0:8} — обновляюсь"
git checkout --quiet main
git pull --ff-only --quiet origin main

echo "$(date -Is) тесты (make ci-test)…"
if ! make ci-test; then
  echo "$(date -Is) ❌ ТЕСТЫ УПАЛИ — деплой отменён (HEAD=${remote_sha:0:8})"
  exit 1
fi

echo "$(date -Is) ✅ тесты ок — деплой (make deploy)…"
make deploy
echo "$(date -Is) 🚀 деплой завершён (HEAD=${remote_sha:0:8})"
