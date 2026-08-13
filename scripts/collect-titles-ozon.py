#!/usr/bin/env python3
"""Собирает названия товаров из выдач Ozon по списку категорий в TSV.

Пара к scripts/title-code-coverage.py: тот считает, насколько названия несут
извлекаемый код модели, а этот поставляет ему данные. См. docs/PRODUCT-MATCH-JVM.md §11.

Ходит через сайдкар ozon-miner (прогретая браузерная дорожка) — прямой запрос к
Ozon отдаёт антибот FAB. Запускать НА ПРОДЕ, наружу у сайдкара портов нет:

    docker exec -i pt_ozon_miner python3 - < scripts/collect-titles-ozon.py > ~/titles.tsv

Разбор повторяет боевой parseSearch (internal/scraper/ozon_search.go): тайлы
лежат в виджетах searchResults*/tileGrid*, у каждого есть ссылка /product/<...>-<id>/
и атом textDS с id=="name". ПЕРВАЯ версия скрипта брала все текстовые атомы
подряд и собрала интерфейс Ozon («Войдите, чтобы делать покупки», футер с ООО),
из-за чего доли поехали — поэтому идём по структуре, а не по регэкспу.
"""
import json
import re
import sys
import time
import urllib.parse
import urllib.request

MINER = "http://localhost:8080"

# Категории подобраны по ожидаемой доле кодов в названии: от техники, где
# артикул производителя обычно есть, до одежды и расходников, где его нет.
QUERIES = [
    "кофемашина",
    "смартфон",
    "наушники беспроводные",
    "робот пылесос",
    "фен для волос",
    "кроссовки мужские",
    "коляска детская",
    "корм для кошек",
]

RE_PRODUCT_LINK = re.compile(r"/product/(?:[^\"/?#]*-)?(\d+)/?")


def fetch(query: str) -> bytes:
    url = f"{MINER}/search?text={urllib.parse.quote(query)}"
    with urllib.request.urlopen(url, timeout=180) as r:
        return r.read()


def walk(node):
    """Обход дерева в ширину без рекурсии — тайлы вложены глубоко и неровно."""
    stack = [node]
    while stack:
        v = stack.pop()
        yield v
        if isinstance(v, dict):
            stack.extend(v.values())
        elif isinstance(v, list):
            stack.extend(v)


def first_text(node):
    for v in walk(node):
        if isinstance(v, dict):
            t = v.get("text")
            if isinstance(t, str) and len(t) > 5:
                return t
    return None


def tile_name(tile):
    """Название тайла — атом с id == "name" (textDS)."""
    for v in walk(tile):
        if isinstance(v, dict) and v.get("id") == "name":
            t = first_text(v)
            if t:
                return t
    return None


def tile_sku(tile):
    for v in walk(tile):
        if isinstance(v, dict):
            link = v.get("link")
            if isinstance(link, str):
                m = RE_PRODUCT_LINK.search(link)
                if m:
                    return m.group(1)
    return None


def widget_rank(name: str) -> int:
    ln = name.lower()
    if "searchresult" in ln:
        return 0
    if "tilegrid" in ln or "tile" in ln or "grid" in ln:
        return 1
    return 2


def titles(raw: bytes):
    try:
        states = json.loads(raw).get("widgetStates", {})
    except Exception as e:  # noqa: BLE001 — диагностика
        print(f"  не разобрался верхний JSON: {e}", file=sys.stderr)
        return []

    out, seen = [], set()
    for key in sorted(states, key=lambda k: (widget_rank(k), k)):
        if widget_rank(key) == 2:
            continue
        try:
            data = json.loads(states[key])
        except Exception:  # noqa: BLE001 — виджет не JSON, пропускаем
            continue
        for v in walk(data):
            if not isinstance(v, dict):
                continue
            for tile in v.get("items", []) or []:
                if not isinstance(tile, dict):
                    continue
                sku = tile_sku(tile)
                name = tile_name(tile)
                if sku and name and sku not in seen:
                    seen.add(sku)
                    out.append(name)
    return out


def main():
    for q in QUERIES:
        try:
            raw = fetch(q)
        except Exception as e:  # noqa: BLE001 — диагностика, не обработка
            print(f"ОШИБКА {q}: {e}", file=sys.stderr)
            continue
        found = titles(raw)
        print(f"{q:<24} тайлов с названием: {len(found)}", file=sys.stderr)
        for t in found:
            print(f"{q}\t\t{t.replace(chr(9), ' ')}")
        sys.stdout.flush()
        time.sleep(5)  # дорожка сайдкара не любит частых запросов


if __name__ == "__main__":
    main()
