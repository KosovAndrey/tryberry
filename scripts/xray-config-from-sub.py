#!/usr/bin/env python3
"""Собрать xray/config.json из подписочной ссылки (vless://... в base64).

Xray-core не умеет подписки — ест только статический config.json. Раньше 6
узлов un1.pro вписывались руками; на подписке в 78 узлов это неподъёмно, плюс
провайдер меняет их без предупреждения. Скрипт разворачивает подписку в набор
outbound'ов под общим balancer'ом (leastPing до Telegram), как в прежнем
конфиге — потребители (api/bot-worker/notifier) ничего не замечают.

Узлов берём не все: observatory пробит каждое плечо раз в probe-interval, а
150 лишних коннектов в минуту ради узлов в Канаде egress'у не помогают. По
умолчанию — 16 штук, разложенных round-robin по странам (сначала близкие к
RU/Telegram), чтобы падение одной локации не выкосило все плечи разом.

Inbound'ов два: :8888 — общий egress (липкое лучшее плечо, под long-poll
Telegram), :8889 — поисковый (случайное плечо на соединение, чтобы лимит
search.wb.ru по IP делился на число узлов). См. spread_balancer().

Использование (на сервере, из корня репо):

    python3 scripts/xray-config-from-sub.py 'https://sub.example/CODE' \
        -o xray/config.json
    docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d xray

Подписка и config.json — секреты, в git не кладём (config.json в .gitignore).
"""

from __future__ import annotations

import argparse
import base64
import binascii
import json
import os
import re
import sys
import urllib.parse
import urllib.request
from typing import Any

# Порядок стран = близость к RU-серверу и к DC Telegram (Амстердам). Балансировщик
# всё равно померит реальный пинг, но выбирать, КОГО мерить, лучше осмысленно.
# NEAR — ближний круг: набираем плечи из него, дальние берём, только если не хватило.
# Флаг → код страны: код уезжает в тег плеча (vless-lt-lt1), чтобы страна читалась
# и в логах, и в regexp'ах весов при --prefer.
CC = {
    "🇫🇮": "fi", "🇪🇪": "ee", "🇱🇻": "lv", "🇱🇹": "lt", "🇸🇪": "se", "🇳🇱": "nl",
    "🇩🇪": "de", "🇵🇱": "pl", "🇩🇰": "dk", "🇦🇹": "at", "🇨🇭": "ch", "🇨🇿": "cz",
    "🇬🇧": "gb", "🇫🇷": "fr", "🇮🇹": "it", "🇹🇷": "tr", "??": "xx",
}
NEAR = ["🇫🇮", "🇪🇪", "🇱🇻", "🇱🇹", "🇸🇪", "🇳🇱", "🇩🇪", "🇵🇱"]
FAR = ["🇩🇰", "🇦🇹", "🇨🇭", "🇨🇿", "🇬🇧", "🇫🇷", "🇮🇹", "🇹🇷"]
COUNTRY_ORDER = NEAR + FAR

DEFAULT_LIMIT = 16
PROBE_URL = "https://api.telegram.org/"

# Веса для --prefer: cost умножает измеренный RTT, поэтому это «мягкий» приоритет.
# Первая страна идёт с 1, дальше ×3 — плечо второго эшелона перебьёт первый, только
# если реально втрое быстрее. Непредпочтённые остаются в пуле с большим весом: они
# не мешают, но спасают, если предпочтённые страны разом лягут.
PREFER_COSTS = [1, 3, 9, 27]
UNPREFERRED_COST = 100


def sub_headers(hwid: str = "") -> dict[str, str]:
    """Заголовки запроса подписки.

    Панели с привязкой к устройству (Remnawave и подобные) вместо узлов отдают
    подсказку «включите отправку HWID», а лимит устройств считают по заголовку
    `x-hwid`. Идентификатор должен быть ПОСТОЯННЫМ: каждый новый занимает слот
    устройства, и на тарифе «1 устройство» второй запрос с новым HWID получает
    «вы достигли максимального количества устройств» (проверено 01-09-2026).
    Поэтому берём его из XRAY_SUB_HWID и держим в .env рядом с самой подпиской.
    """
    headers = {"User-Agent": "v2rayN/7.12.5"}
    hwid = hwid or os.getenv("XRAY_SUB_HWID", "").strip()
    if hwid:
        headers.update({
            "x-hwid": hwid,
            "x-device-os": "Windows",
            "x-ver-os": "11",
            "x-device-model": "tryberrybot-prod",
        })
    return headers


def fetch(src: str, hwid: str = "") -> str:
    """Скачать подписку (или прочитать локальный файл — удобно для отладки)."""
    if src.startswith(("http://", "https://")):
        req = urllib.request.Request(src, headers=sub_headers(hwid))
        with urllib.request.urlopen(req, timeout=30) as resp:
            return resp.read().decode("utf-8", "replace")
    with open(src, encoding="utf-8") as fh:
        return fh.read()


def decode(payload: str) -> list[str]:
    """Подписка приходит base64; некоторые панели отдают plain-текст."""
    text = payload.strip()
    if "vless://" not in text:
        pad = "=" * (-len(text) % 4)
        try:
            text = base64.b64decode(text + pad).decode("utf-8", "replace")
        except (binascii.Error, ValueError) as err:
            sys.exit(f"подписка не base64 и не содержит vless://: {err}")
    return [ln.strip() for ln in text.splitlines() if ln.strip().startswith("vless://")]


def parse(link: str) -> dict[str, Any] | None:
    """vless://UUID@host:port?params#name → плоский dict. Не-reality пропускаем."""
    u = urllib.parse.urlparse(link)
    q = dict(urllib.parse.parse_qsl(u.query))
    if q.get("security") != "reality" or not u.hostname or not u.username:
        return None
    return {
        "uuid": u.username,
        "host": u.hostname,
        "port": u.port or 443,
        "name": urllib.parse.unquote(u.fragment),
        "type": q.get("type", "tcp"),
        "sni": q.get("sni") or u.hostname,
        "fp": q.get("fp") or "chrome",
        "pbk": q.get("pbk", ""),
        "sid": q.get("sid", ""),
        "spx": q.get("spx", ""),
        "flow": q.get("flow", ""),
        "service": q.get("serviceName", ""),
        "path": q.get("path", ""),
    }


def slug(node: dict[str, Any], used: set[str]) -> str:
    """Тег плеча. Селектор балансировщика матчит по префиксу — обязателен vless-."""
    # У хостнейма достаточно первой метки (fl1, ee10, de7); голый IP берём целиком —
    # 31.59.45.26 и 31.59.45.185 по первому октету не различить.
    is_ip = re.fullmatch(r"[\d.]+", node["host"]) is not None
    head = node["host"] if is_ip else node["host"].split(".")[0]
    base = re.sub(r"[^a-z0-9]+", "-", head.lower()).strip("-") or "node"
    if is_ip:
        base = "ip-" + base
    cc = CC.get(country(node), "xx")
    tag, n = f"vless-{cc}-{base}", 2
    while tag in used:
        tag, n = f"vless-{cc}-{base}-{n}", n + 1
    used.add(tag)
    return tag


def country(node: dict[str, Any]) -> str:
    """Флаг из названия узла — им же группируем. Без флага = хвост списка."""
    for flag in COUNTRY_ORDER:
        if flag in node["name"]:
            return flag
    return "??"


def pick(nodes: list[dict[str, Any]], limit: int,
         prefer: list[str] | None = None) -> list[dict[str, Any]]:
    """Round-robin по странам ближнего круга; дальние — только на добор.

    Подряд идущие fl1..fl10 — это одна локация: выпадет она, выпадут все. Обход
    по кругу даёт географический разброс при том же числе плеч.

    При --prefer предпочтённые страны идут первым эшелоном: им достаётся больше
    плеч, а веса в балансировщике довершают дело.
    """
    buckets: dict[str, list[dict[str, Any]]] = {}
    for node in nodes:
        buckets.setdefault(country(node), []).append(node)

    def fill(tiers: list[list[str]], upto: int) -> None:
        for tier in tiers:
            order = [c for c in tier if buckets.get(c)]
            while order and len(out) < upto:
                for flag in list(order):
                    if not buckets[flag]:
                        order.remove(flag)
                        continue
                    out.append(buckets[flag].pop(0))
                    if len(out) >= upto:
                        break
            if len(out) >= upto:
                return

    out: list[dict[str, Any]] = []
    if not prefer:
        fill([NEAR, FAR + ["??"]], limit)
        return out

    # Предпочтённым странам — основная масса плеч, но не все: пара мест держится за
    # остальными. Именно так лёг egress 30-07 — умер весь провайдер разом, и пул из
    # одних только «любимых» стран умер бы с ним.
    head = [f for f in COUNTRY_ORDER if CC.get(f) in prefer]
    rest = [[f for f in t if f not in head] for t in (NEAR, FAR + ["??"])]
    reserve = max(2, limit // 5)
    fill([head], limit - reserve)
    fill(rest, limit)
    fill([head], limit)  # непредпочтённых не хватило — добираем своими
    return out


def outbound(node: dict[str, Any], tag: str) -> dict[str, Any]:
    stream: dict[str, Any] = {
        "network": node["type"],
        "security": "reality",
        "realitySettings": {
            "show": False,
            "fingerprint": node["fp"],
            "serverName": node["sni"],
            "publicKey": node["pbk"],
            "shortId": node["sid"],
            "spiderX": node["spx"],
        },
    }
    if node["type"] == "grpc":
        stream["grpcSettings"] = {"serviceName": node["service"], "multiMode": False}
    elif node["type"] == "xhttp":
        # xhttp (aka splithttp) ядро зовёт "xhttp" начиная с Xray 24.11.
        stream["xhttpSettings"] = {"path": node["path"] or "/", "mode": "auto"}
    # flow=xtls-rprx-vision живёт только на raw/tcp; на grpc/xhttp ядро ругнётся.
    flow = node["flow"] if node["type"] == "tcp" else ""
    return {
        "tag": tag,
        "//": node["name"],
        "protocol": "vless",
        "settings": {
            "vnext": [{
                "address": node["host"],
                "port": node["port"],
                "users": [{"id": node["uuid"], "encryption": "none", "flow": flow}],
            }]
        },
        "streamSettings": stream,
    }


def watcher(prefer: list[str] | None, probe_interval: str) -> dict[str, Any]:
    """Чем мерить плечи. leastPing читает observatory, leastLoad — burstObservatory.

    Ставим ровно один: лишний живой пробер — это лишние коннекты на каждое плечо
    и лишний повод спалить паттерн трафика.
    """
    if not prefer:
        return {"observatory": {
            "//": "Пробит все vless-* — живость и пинг до Telegram для балансировщика.",
            "subjectSelector": ["vless"],
            "probeUrl": PROBE_URL,
            "probeInterval": probe_interval,
        }}
    return {"burstObservatory": {
        "//": "leastLoad считает по выборке замеров burstObservatory, не observatory.",
        "subjectSelector": ["vless"],
        "pingConfig": {
            "destination": PROBE_URL,
            "interval": probe_interval,
            "timeout": "5s",
            "sampling": 5,
            "httpMethod": "GET",
        },
    }}


def balancer(prefer: list[str] | None) -> dict[str, Any]:
    """Без --prefer — «самое быстрое живое». С --prefer — веса по странам.

    В Xray нет стратегии «строгий порядок стран»: fallbackTag принимает outbound,
    а не другой балансировщик, цепочку lt→ee→de им не выразить. Настройки есть
    только у leastLoad, где cost умножает измеренный RTT — это и даёт приоритет,
    но мягкий: перегруженное плечо предпочтённой страны всё же уступит быстрому
    чужому, а не будет держать трафик до последнего.
    """
    if not prefer:
        return {
            "//": "leastPing держит трафик на живом и самом быстром плече.",
            "tag": "egress",
            "selector": ["vless"],
            "strategy": {"type": "leastPing"},
        }
    costs = [{"regexp": True, "match": f"^vless-{cc}-", "value": value}
             for cc, value in zip(prefer, PREFER_COSTS)]
    costs.append({"regexp": True, "match": "^vless-", "value": UNPREFERRED_COST})
    return {
        "//": f"Приоритет стран: {' > '.join(prefer)} (cost умножает RTT, меньше = лучше).",
        "tag": "egress",
        "selector": ["vless"],
        # expected=1 — держать РОВНО одно плечо. При 2+ ядро раскидывает соединения
        # между лучшими, а с DisableKeepAlives у бота это значит новый путь почти на
        # каждый запрос: трафик скачет lt1→de9→ee7 и никуда не «прогревается».
        "strategy": {"type": "leastLoad", "settings": {"expected": 1, "costs": costs}},
    }


def spread_balancer() -> dict[str, Any]:
    """Балансировщик для поискового плеча: РАЗНЫЙ узел на каждое соединение.

    Основной egress намеренно липкий (одно плечо: long-poll к Telegram любит
    стабильный путь). Поиску нужно ровно обратное: search.wb.ru лимитирует по
    IP, и весь поиск с одного узла упирается в 429 — замер 24–27.08 дал 44 832
    ответа 429 против 30 751 успешных, притом что 403 после ухода с
    __internal исчезли совсем. random раскидывает соединения по всем плечам,
    и лимит делится на их число.

    random, а не leastPing/leastLoad: обе «умные» стратегии сходятся на лучшем
    узле — то есть ровно на том, чего мы здесь избегаем. Мёртвое плечо ловится
    ретраем на стороне скрейпера (fetchPage перебирает попытки), а не пробером.
    """
    return {
        "//": "Поисковый egress: случайное плечо на соединение — лимит WB делится на число узлов.",
        "tag": "egress-spread",
        "selector": ["vless"],
        "strategy": {"type": "random"},
    }


def build(nodes: list[dict[str, Any]], probe_interval: str,
          prefer: list[str] | None = None) -> dict[str, Any]:
    used: set[str] = set()
    outbounds = [outbound(n, slug(n, used)) for n in nodes]
    outbounds.append({"tag": "direct", "protocol": "freedom"})
    return {
        "//": "СГЕНЕРИРОВАНО scripts/xray-config-from-sub.py — правки затрёт следующий прогон.",
        "//1": "Секрет (UUID/pbk внутри): xray/config.json в .gitignore. См. xray/README.md.",
        "log": {"loglevel": "warning"},
        "inbounds": [{
            "//": "Локальный HTTP-прокси. Сюда ходят api/bot-worker/notifier (HTTPS_PROXY).",
            "tag": "in",
            "listen": "0.0.0.0",
            "port": 8888,
            "protocol": "http",
            "sniffing": {"enabled": True, "destOverride": ["http", "tls"]},
        }, {
            "//": "Поисковый прокси: SEARCH_PROXY_URLS=http://xray:8889 у search/reseller-worker.",
            "tag": "in-search",
            "listen": "0.0.0.0",
            "port": 8889,
            "protocol": "http",
            "sniffing": {"enabled": True, "destOverride": ["http", "tls"]},
        }],
        "outbounds": outbounds,
        **watcher(prefer, probe_interval),
        "routing": {
            "domainStrategy": "AsIs",
            "balancers": [balancer(prefer), spread_balancer()],
            # Порядок правил значим: первое совпавшее выигрывает, поэтому
            # поисковый inbound перехватываем ДО общего правила.
            "rules": [
                {"type": "field", "inboundTag": ["in-search"], "balancerTag": "egress-spread"},
                {"type": "field", "network": "tcp,udp", "balancerTag": "egress"},
            ],
        },
    }


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("source", help="URL подписки или путь к файлу с vless://-ссылками")
    ap.add_argument("-o", "--out", default="-", help="куда писать конфиг (по умолчанию stdout)")
    ap.add_argument("-n", "--limit", type=int, default=DEFAULT_LIMIT,
                    help=f"сколько плеч оставить (по умолчанию {DEFAULT_LIMIT}, 0 = все)")
    ap.add_argument("--include", help="regex: оставить только узлы, чьё имя/хост совпали")
    ap.add_argument("--exclude", help="regex: выкинуть узлы, чьё имя/хост совпали")
    ap.add_argument("--network", action="append", choices=["tcp", "grpc", "xhttp"],
                    help="оставить только узлы с этим транспортом (можно повторять)")
    ap.add_argument("--prefer", help="приоритет стран через запятую, напр. lt,ee,de — "
                                     "переключает балансировщик на leastLoad с весами")
    ap.add_argument("--probe-interval", default="30s", help="как часто observatory пробит плечи")
    ap.add_argument("--list", action="store_true", help="только показать узлы подписки и выйти")
    ap.add_argument("--hwid", default="", help="идентификатор устройства для панелей с HWID-привязкой "
                                               "(или XRAY_SUB_HWID); должен быть ПОСТОЯННЫМ — каждый новый "
                                               "занимает слот устройства")
    args = ap.parse_args()

    prefer: list[str] = []
    if args.prefer:
        prefer = [c.strip().lower() for c in args.prefer.split(",") if c.strip()]
        known = set(CC.values())
        if bad := [c for c in prefer if c not in known]:
            sys.exit(f"неизвестные коды стран в --prefer: {', '.join(bad)} "
                     f"(есть: {', '.join(sorted(known - {'xx'}))})")
        if len(prefer) > len(PREFER_COSTS):
            sys.exit(f"--prefer поддерживает до {len(PREFER_COSTS)} стран: дальше веса "
                     "перестают что-либо значить, проще сузить пул через --include")

    links = decode(fetch(args.source, args.hwid))
    nodes = [n for n in (parse(l) for l in links) if n]
    if not nodes:
        sys.exit("в подписке нет VLESS+Reality узлов")

    if args.network:
        nodes = [n for n in nodes if n["type"] in args.network]
        if not nodes:
            sys.exit("после --network не осталось узлов")

    for flag, rx, keep in (("--include", args.include, True), ("--exclude", args.exclude, False)):
        if rx:
            pat = re.compile(rx, re.I)
            nodes = [n for n in nodes
                     if bool(pat.search(n["name"]) or pat.search(n["host"])) is keep]
            if not nodes:
                sys.exit(f"после {flag} не осталось узлов")

    if args.list:
        for n in nodes:
            print(f'{n["host"]:34s}:{n["port"]:<5d} {n["type"]:5s} {country(n)} {n["name"]}')
        return

    chosen = pick(nodes, args.limit, prefer) if args.limit else nodes
    config = build(chosen, args.probe_interval, prefer)
    text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"

    if args.out == "-":
        sys.stdout.write(text)
    else:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(text)
    strategy = f"leastLoad, приоритет {' > '.join(prefer)}" if prefer else "leastPing"
    print(f"узлов в подписке: {len(nodes)}, в конфиге: {len(chosen)} → {args.out} [{strategy}]",
          file=sys.stderr)
    used: set[str] = set()
    for n in chosen:
        print(f'  {country(n)} {slug(n, used):26s} {n["host"]}:{n["port"]} ({n["type"]})',
              file=sys.stderr)


if __name__ == "__main__":
    main()
