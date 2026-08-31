#!/usr/bin/env python3
"""Чем именно WB режет поиск: матрица эндпоинтов и заголовков через один прокси.

Когда search.wb.ru отвечает 429 со ВСЕХ плеч сразу (замер 31-08: 0 чистых из 35
адресов в 17 подсетях), вопрос уже не «какой IP взять», а «что это за лимит».
Ответ обычно лежит в самом ответе: Retry-After, имя фронта в Server, текст тела.
Плюс полезно знать, режется ли только поисковый эндпоинт или весь домен — если
карточка с того же адреса отдаёт 200, значит дело в ручке, а не в репутации IP.

Скрипт гоняет матрицу «эндпоинт × набор заголовков» через ОДИН прокси и печатает
статус, интересные заголовки ответа и начало тела. Запросы идут ИЗНУТРИ
контейнера (по умолчанию pt_wb_search_miner), потому что xray доступен только по
имени во внутренней сети compose.

Примеры:
    python3 scripts/wb-endpoint-probe.py --proxy http://xray:8888
    python3 scripts/wb-endpoint-probe.py --proxy http://xray:8889 --repeat 3
    python3 scripts/wb-endpoint-probe.py --proxy '' --container pt_scraper   # direct
"""

from __future__ import annotations

import argparse
import json
import subprocess
import sys

QUERY = "%D0%BA%D0%B0%D0%BF%D0%B8%D0%B1%D0%B0%D1%80%D0%B0"  # «капибара», холодный запрос
COMMON = f"query={QUERY}&resultset=catalog&curr=rub&dest=-1257786&spp=30&page=1"

# Эндпоинты: текущий боевой, соседние версии/хосты и КОНТРОЛЬ — карточка и
# статика. Контроль отвечает на вопрос «это лимит на поиск или на весь WB».
ENDPOINTS = [
    ("search v18 (боевой)", f"https://search.wb.ru/exactmatch/ru/common/v18/search?{COMMON}"),
    ("search v13", f"https://search.wb.ru/exactmatch/ru/common/v13/search?{COMMON}"),
    ("search v18 другой dest", f"https://search.wb.ru/exactmatch/ru/common/v18/search?"
                               f"query={QUERY}&resultset=catalog&curr=rub&dest=123585924&spp=30&page=1"),
    ("u-search v18", f"https://u-search.wb.ru/exactmatch/ru/common/v18/search?{COMMON}"),
    ("КОНТРОЛЬ карточка", "https://card.wb.ru/cards/v4/detail?appType=1&curr=rub"
                          "&dest=-1257786&spp=30&nm=196602569"),
    ("КОНТРОЛЬ статика", "https://static-basket-01.wbbasket.ru/vol0/data/subject-base.json"),
]

# Наборы заголовков: голый urllib против браузерного. Если 429 уходит на
# браузерном наборе — лимит про фингерпринт запроса, а не про адрес.
HEADER_SETS = {
    "голый": {},
    "браузерный": {
        "User-Agent": "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
                      "(KHTML, like Gecko) Chrome/140.0.0.0 Safari/537.36",
        "Accept": "*/*",
        "Accept-Language": "ru-RU,ru;q=0.9",
        "Origin": "https://www.wildberries.ru",
        "Referer": "https://www.wildberries.ru/",
        "Sec-Fetch-Dest": "empty",
        "Sec-Fetch-Mode": "cors",
        "Sec-Fetch-Site": "cross-site",
    },
}

# Тело пробы исполняется внутри контейнера: параметры прилетают JSON'ом через
# argv, чтобы не мучить кавычки и проценты в URL.
PROBE_SRC = r"""
import json, sys, urllib.request
cfg = json.loads(sys.argv[1])
proxy = cfg["proxy"]
opener = urllib.request.build_opener(
    urllib.request.ProxyHandler({"https": proxy, "http": proxy} if proxy else {}))
KEEP = ("retry-after", "server", "x-ratelimit-remaining", "x-ratelimit-limit",
        "x-kong-response-latency", "cf-ray", "content-type")
out = []
for name, url, headers in cfg["cases"]:
    row = {"name": name, "status": None, "note": "", "headers": {}, "body": ""}
    req = urllib.request.Request(url, headers=headers)
    try:
        r = opener.open(req, timeout=cfg["timeout"])
        body = r.read(400)
        row["status"] = r.status
        row["headers"] = {k: v for k, v in r.headers.items() if k.lower() in KEEP}
        row["body"] = body[:160].decode("utf-8", "replace")
    except urllib.error.HTTPError as e:
        row["status"] = e.code
        row["headers"] = {k: v for k, v in e.headers.items() if k.lower() in KEEP}
        row["body"] = e.read(160).decode("utf-8", "replace")
    except Exception as e:
        row["note"] = f"{type(e).__name__}: {str(e)[:60]}"
    out.append(row)
print(json.dumps(out, ensure_ascii=False))
"""


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--proxy", default="http://xray:8888",
                    help="прокси для запросов (пустая строка = direct)")
    ap.add_argument("--container", default="pt_wb_search_miner",
                    help="контейнер с python, изнутри которого стучимся")
    ap.add_argument("--repeat", type=int, default=1, help="сколько раз прогнать матрицу")
    ap.add_argument("--timeout", type=float, default=15.0)
    ap.add_argument("--headers", choices=sorted(HEADER_SETS) + ["все"], default="все")
    args = ap.parse_args()

    sets = HEADER_SETS if args.headers == "все" else {args.headers: HEADER_SETS[args.headers]}
    cases = [(f"{hname:11} {ename}", url, hdrs)
             for hname, hdrs in sets.items()
             for ename, url in ENDPOINTS]

    payload = {"proxy": args.proxy, "timeout": args.timeout, "cases": cases}
    print(f"прокси: {args.proxy or 'direct'} | кейсов {len(cases)} | прогонов {args.repeat}\n")

    for run in range(1, args.repeat + 1):
        res = subprocess.run(
            ["docker", "exec", "-i", args.container, "python", "-c", PROBE_SRC,
             json.dumps(payload)],
            capture_output=True, text=True)
        if res.returncode != 0:
            sys.exit("проба не запустилась: " + (res.stderr.strip()[:300] or "нет вывода"))
        try:
            rows = json.loads(res.stdout.strip().splitlines()[-1])
        except (ValueError, IndexError):
            sys.exit("не разобрал вывод пробы: " + res.stdout[:300])

        if args.repeat > 1:
            print(f"── прогон {run} ──")
        for row in rows:
            status = row["note"] or row["status"]
            hdrs = " ".join(f"{k}={v}" for k, v in row["headers"].items())
            print(f"{str(status):>6}  {row['name']}")
            if hdrs:
                print(f"        {hdrs}")
            if row["body"].strip():
                print(f"        тело: {row['body'].strip()[:140]}")
        print()


if __name__ == "__main__":
    main()
