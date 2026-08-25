#!/usr/bin/env python3
"""Сверка ключей окружения: что код читает через os.getenv vs что проброшено в compose.

Зачем: ключ есть в коде, но не проброшен в compose — значит настройки как будто
нет, и покрутить её в инциденте нельзя без пересборки. Обратный случай хуже:
ключ есть в compose, но код его не читает — конфиг врёт про поведение системы
(так MINE_INTERVAL_HOURS="12" создавал уверенность, что майнер ходит в браузер
раз в 12 часов, тогда как он просыпается каждые 5 минут).

Запуск: python3 scripts/env-audit.py [--all]
  без флага — только значимые расхождения (мёртвые ключи + непроброшенные,
  кроме косметических), с --all — полный список.
"""
import glob
import os
import re
import sys

import yaml

ROOT = os.path.dirname(os.path.dirname(os.path.abspath(__file__)))
COMPOSE = os.path.join(ROOT, "docker-compose.yml")

# сервис compose -> каталог с кодом
PY_SERVICES = {
    "ali-miner": "ali-miner",
    "ozon-miner": "ozon-miner",
    "wb-search-miner": "wb-search-miner",
    "token-miner": "token-miner",
    "token-miner-reseller": "token-miner",
}

# Ключи, которые код собирает динамически (os.getenv(f"..._{i}_...")) — regex их
# не ловит, и они НЕ мёртвые.
DYNAMIC = re.compile(r"^(WB|ALI|OZON)_LANE_\d+_(PROXY|COOKIE)$")

# Косметика и отладка: жить с дефолтом кода нормально, в compose не тащим.
COSMETIC = re.compile(
    r"(LOCALE|TIMEZONE|USER_AGENT|_MARKER|DEBUG|DISPLAY|BROWSER_CHANNEL|BLOCK_RESOURCES)"
)

GETENV = re.compile(r'os\.getenv\(\s*["\']([A-Z0-9_]+)["\']')


def code_keys(directory: str) -> set:
    keys = set()
    for f in glob.glob(os.path.join(ROOT, directory, "*.py")):
        if os.path.basename(f).startswith(("probe", "test_")):
            continue
        keys |= set(GETENV.findall(open(f, encoding="utf-8").read()))
    return keys


def main() -> int:
    show_all = "--all" in sys.argv
    show_unwired = show_all or "--unwired" in sys.argv
    compose = yaml.safe_load(open(COMPOSE, encoding="utf-8"))
    problems = 0

    for svc, directory in PY_SERVICES.items():
        in_code = code_keys(directory)
        in_compose = set((compose["services"].get(svc, {}).get("environment") or {}).keys())

        dead = sorted(k for k in in_compose - in_code if not DYNAMIC.match(k))
        unwired = sorted(k for k in in_code - in_compose if show_all or not COSMETIC.search(k))

        if not dead and not (unwired and show_unwired):
            continue
        print(f"\n{svc}  (код: {directory}/)")
        for k in dead:
            problems += 1
            print(f"  МЁРТВЫЙ  {k} — задан в compose, код его не читает")
        if show_unwired:
            for k in unwired:
                print(f"  не проброшен  {k}")

    # Непроброшенный ключ — не всегда беда: у части настроек дефолт кода и есть
    # ответ. Мёртвый — всегда беда: конфиг утверждает то, чего нет. Поэтому
    # ненулевой код возврата только на мёртвых.
    print("\nМёртвых ключей:", problems, "(список непроброшенных: --unwired)")
    return 1 if problems else 0


if __name__ == "__main__":
    sys.exit(main())
