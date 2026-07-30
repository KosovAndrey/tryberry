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
import re
import sys
import urllib.parse
import urllib.request
from typing import Any

# Порядок стран = близость к RU-серверу и к DC Telegram (Амстердам). Балансировщик
# всё равно померит реальный пинг, но выбирать, КОГО мерить, лучше осмысленно.
# NEAR — ближний круг: набираем плечи из него, дальние берём, только если не хватило.
NEAR = ["🇫🇮", "🇪🇪", "🇱🇻", "🇱🇹", "🇸🇪", "🇳🇱", "🇩🇪", "🇵🇱"]
FAR = ["🇩🇰", "🇦🇹", "🇨🇭", "🇨🇿", "🇬🇧", "🇫🇷", "🇮🇹", "🇹🇷"]
COUNTRY_ORDER = NEAR + FAR

DEFAULT_LIMIT = 16
PROBE_URL = "https://api.telegram.org/"


def fetch(src: str) -> str:
    """Скачать подписку (или прочитать локальный файл — удобно для отладки)."""
    if src.startswith(("http://", "https://")):
        req = urllib.request.Request(src, headers={"User-Agent": "v2rayNG/1.8.0"})
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
    tag, n = f"vless-{base}", 2
    while tag in used:
        tag, n = f"vless-{base}-{n}", n + 1
    used.add(tag)
    return tag


def country(node: dict[str, Any]) -> str:
    """Флаг из названия узла — им же группируем. Без флага = хвост списка."""
    for flag in COUNTRY_ORDER:
        if flag in node["name"]:
            return flag
    return "??"


def pick(nodes: list[dict[str, Any]], limit: int) -> list[dict[str, Any]]:
    """Round-robin по странам ближнего круга; дальние — только на добор.

    Подряд идущие fl1..fl10 — это одна локация: выпадет она, выпадут все. Обход
    по кругу даёт географический разброс при том же числе плеч.
    """
    buckets: dict[str, list[dict[str, Any]]] = {}
    for node in nodes:
        buckets.setdefault(country(node), []).append(node)

    out: list[dict[str, Any]] = []
    for tier in (NEAR, FAR + ["??"]):
        order = [c for c in tier if buckets.get(c)]
        while order and len(out) < limit:
            for flag in list(order):
                if not buckets[flag]:
                    order.remove(flag)
                    continue
                out.append(buckets[flag].pop(0))
                if len(out) >= limit:
                    break
        if len(out) >= limit:
            break
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


def build(nodes: list[dict[str, Any]], probe_interval: str) -> dict[str, Any]:
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
        }],
        "outbounds": outbounds,
        "observatory": {
            "//": "Пробит все vless-* — живость и пинг до Telegram для балансировщика.",
            "subjectSelector": ["vless"],
            "probeUrl": PROBE_URL,
            "probeInterval": probe_interval,
        },
        "routing": {
            "domainStrategy": "AsIs",
            "balancers": [{
                "//": "leastPing держит трафик на живом и самом быстром плече.",
                "tag": "egress",
                "selector": ["vless"],
                "strategy": {"type": "leastPing"},
            }],
            "rules": [{"type": "field", "network": "tcp,udp", "balancerTag": "egress"}],
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
    ap.add_argument("--probe-interval", default="30s", help="как часто observatory пробит плечи")
    ap.add_argument("--list", action="store_true", help="только показать узлы подписки и выйти")
    args = ap.parse_args()

    links = decode(fetch(args.source))
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

    chosen = pick(nodes, args.limit) if args.limit else nodes
    config = build(chosen, args.probe_interval)
    text = json.dumps(config, ensure_ascii=False, indent=2) + "\n"

    if args.out == "-":
        sys.stdout.write(text)
    else:
        with open(args.out, "w", encoding="utf-8") as fh:
            fh.write(text)
    print(f"узлов в подписке: {len(nodes)}, в конфиге: {len(chosen)} → {args.out}",
          file=sys.stderr)
    for n in chosen:
        print(f'  {country(n)} {n["host"]}:{n["port"]} ({n["type"]})', file=sys.stderr)


if __name__ == "__main__":
    main()
