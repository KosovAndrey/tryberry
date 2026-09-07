#!/usr/bin/env python3
"""
Докачка дефолтных аддонов camoufox на этапе СБОРКИ — с проверкой и падением билда.

Зачем отдельный шаг (инцидент 07-09-2026): `python -m camoufox fetch` тянет uBO с
addons.mozilla.org, и неудачу закачки camoufox только ПЕЧАТАЕТ ("Failed to download
and extract UBO"), оставляя пустой каталог addons/UBO. Дальше это тихая мина:
maybe_download_addons считает аддон готовым (каталог существует), а confirm_paths
при запуске браузера падает на "manifest.json is missing" — и НИ ОДНА дорожка не
поднимается. Образ собрался «успешно», пул мёртв целиком.

Здесь: битые (пустые) каталоги сносим, качаем с ретраями, а если после них аддон
всё ещё не распакован — валим сборку. Лучше не собраться, чем собрать образ,
который не поднимет ни одного браузера.
"""
import os
import shutil
import sys
import time

from camoufox.addons import DefaultAddons, get_addon_path, maybe_download_addons

ATTEMPTS = 3
PAUSE_S = 5


def extracted(addon) -> bool:
    return os.path.exists(os.path.join(get_addon_path(addon.name), "manifest.json"))


def drop_broken(addon) -> None:
    """Пустой/недокачанный каталог — именно он обманывает maybe_download_addons."""
    path = get_addon_path(addon.name)
    if os.path.isdir(path) and not extracted(addon):
        print(f"аддон {addon.name}: каталог есть, manifest.json нет — сношу {path}")
        shutil.rmtree(path, ignore_errors=True)


def main() -> int:
    addons = list(DefaultAddons)
    for attempt in range(1, ATTEMPTS + 1):
        for addon in addons:
            drop_broken(addon)
        maybe_download_addons(addons)
        missing = [a.name for a in addons if not extracted(a)]
        if not missing:
            print(f"аддоны распакованы: {', '.join(a.name for a in addons)}")
            return 0
        print(f"попытка {attempt}/{ATTEMPTS}: не распакованы {missing}")
        if attempt < ATTEMPTS:
            time.sleep(PAUSE_S)
    print(f"аддоны camoufox не скачались за {ATTEMPTS} попытки — сборка остановлена",
          file=sys.stderr)
    return 1


if __name__ == "__main__":
    sys.exit(main())
