#!/usr/bin/env python3
"""Собирает названия товаров из выдач Ozon по списку категорий в TSV.

Пара к scripts/title-code-coverage.py: тот считает, насколько названия несут
извлекаемый код модели, а этот поставляет ему данные. См. docs/PRODUCT-MATCH-JVM.md §11.

Ходит через сайдкар ozon-miner (прогретая браузерная дорожка) — прямой запрос к
Ozon отдаёт антибот FAB. Запускать НА ПРОДЕ, изнутри контейнера сайдкара наружу
портов нет:

    docker exec -i pt_ozon_miner python3 - < scripts/collect-titles-ozon.py > /tmp/titles.tsv

Названия достаём эвристикой, а не парсером: widgetStates у Ozon — словарь, где
значения сами по себе JSON-СТРОКИ (кавычки внутри экранированы), поэтому сперва
раскавычиваем, а затем берём текстовые атомы подходящей длины с кириллицей.
Для оценки доли кодов этого достаточно; для боевого разбора есть parseSearch.
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

# Длина отсекает цены, бейджи и служебные подписи; кириллица — навигацию и
# идентификаторы. Порог 25 подобран по живому дампу выдачи.
RE_TEXT = re.compile(r'"text":"([^"]{25,160})"')
RE_CYR = re.compile(r"[а-яА-ЯёЁ]")


def fetch(query: str) -> str:
    url = f"{MINER}/search?text={urllib.parse.quote(query)}"
    with urllib.request.urlopen(url, timeout=180) as r:
        return r.read().decode("utf-8", "replace")


def titles(raw: str):
    unescaped = raw.replace('\\"', '"').replace("\\/", "/")
    seen, out = set(), []
    for m in RE_TEXT.finditer(unescaped):
        t = m.group(1).strip()
        if not RE_CYR.search(t) or t in seen:
            continue
        seen.add(t)
        out.append(t)
    return out


def main():
    for q in QUERIES:
        try:
            raw = fetch(q)
        except Exception as e:  # noqa: BLE001 — диагностика, не обработка
            print(f"ОШИБКА {q}: {e}", file=sys.stderr)
            continue
        found = titles(raw)
        print(f"{q:<24} названий: {len(found)}", file=sys.stderr)
        for t in found:
            t = t.replace("\t", " ")
            print(f"{q}\t\t{t}")
        sys.stdout.flush()
        time.sleep(5)  # дорожка сайдкара не любит частых запросов


if __name__ == "__main__":
    main()
