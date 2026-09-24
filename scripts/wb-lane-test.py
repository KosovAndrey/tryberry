#!/usr/bin/env python3
"""Поштучная проверка плеч xray против search.wb.ru.

Зачем. WB лимитирует публичный search.wb.ru по адресу выхода, и балансировщик
этого не показывает: через общий inbound видно только «429 или не 429», а какое
именно плечо ответило — нет. Скрипт поднимает ВРЕМЕННЫЙ xray на ОДНО плечо,
стучится через него в WB и печатает вердикт по каждому адресу. Отсюда видно,
режет WB по IP или по всей /24, и какие подсети ещё чистые.

Порядок работы:
  1. берём список узлов (из подписки через xray-config-from-sub.py или из
     готового конфига);
  2. TCP-проба — отсеиваем мёртвые узлы (после смены провайдера их много);
  3. схлопываем узлы по резолвнутому IP: один адрес на разных портах — для WB
     ОДИН адрес, тестировать его 19 раз бессмысленно;
  4. каждый оставшийся адрес проверяем в WB через одноразовый контейнер xray.

Примеры:
    python3 scripts/wb-lane-test.py --sub 'https://ПОДПИСКА/КОД'
    python3 scripts/wb-lane-test.py --lanes /tmp/xray-all.json
    python3 scripts/wb-lane-test.py --lanes xray/config.json --requests 5

Результат: таблица по адресам, сводка по /24 и JSON в --out (по умолчанию
/tmp/wb-lane-results.json) — из него собирается конфиг победителей.
"""

from __future__ import annotations

import argparse
import json
import os
import socket
import subprocess
import sys
import time
from collections import Counter, defaultdict
from typing import Any

HERE = os.path.dirname(os.path.abspath(__file__))
GENERATOR = os.path.join(HERE, "xray-config-from-sub.py")

# Холодный запрос: на горячих («iphone 17») WB отвечает 403 по другой причине —
# нам нужен чистый сигнал про rate-limit, а не про антибот.
WB_QS = ("query=%D0%BA%D0%B0%D0%BF%D0%B8%D0%B1%D0%B0%D1%80%D0%B0"
         "&resultset=catalog&curr=rub&dest=-1257786&spp=30&page=1")
# По умолчанию бьём в БОЕВУЮ ручку: с 01-09 это u-search.wb.ru (search.wb.ru
# отдаёт 429 всем подряд). Другую ручку задаёт --url.
WB_URL = f"https://u-search.wb.ru/exactmatch/ru/common/v18/search?{WB_QS}"

# Проба запускается ВНУТРИ контейнера с python (сеть compose), потому что
# временный xray портов наружу не публикует — он доступен только по имени.
PROBE_SRC = """
import urllib.request, collections, time
c = collections.Counter()
for _i in range(__N__):
    if _i and __DELAY__:
        time.sleep(__DELAY__)
    o = urllib.request.build_opener(urllib.request.ProxyHandler(
        {"https": "__PROXY__", "http": "__PROXY__"}))
    try:
        r = o.open("__URL__", timeout=12); r.read(); c[str(r.status)] += 1
    except Exception as e:
        c[str(e)[:24]] += 1
# Страна ВЫХОДА через это же плечо. Адрес узла про неё ничего не говорит: у
# каскадов вход в РФ, выход за границей, а бывает и выход прямо в РФ. WB с
# российского выхода отвечает 200, а Telegram душит ТСПУ (24.09 leastPing
# сел на такое плечо, и бот час ловил TLS handshake timeout). По этому полю
# xray-lanes-pick.py уводит плечо в пул только для поиска (:8889).
try:
    o = urllib.request.build_opener(urllib.request.ProxyHandler(
        {"https": "__PROXY__", "http": "__PROXY__"}))
    c["exit"] = o.open("https://ipinfo.io/country", timeout=12).read().decode().strip()
except Exception:
    c["exit"] = "?"
print(dict(c))
"""


def sh(*args: str) -> subprocess.CompletedProcess:
    return subprocess.run(args, capture_output=True, text=True)


def load_lanes(path: str) -> list[dict[str, Any]]:
    """Плечи из конфига xray или из списка, сохранённого прошлым прогоном."""
    data = json.load(open(path))
    outbounds = data["outbounds"] if isinstance(data, dict) else data
    return [o for o in outbounds if str(o.get("tag", "")).startswith(("vless", "ruexit-"))]


def resolve(lane: dict[str, Any]) -> str | None:
    host = lane["settings"]["vnext"][0]["address"]
    try:
        return socket.gethostbyname(host)
    except OSError:
        return None


def tcp_alive(ip: str, port: int, timeout: float = 5.0) -> bool:
    s = socket.socket()
    s.settimeout(timeout)
    try:
        s.connect((ip, port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def dedup_by_ip(lanes: list[dict[str, Any]], skip_tcp: bool, per_port: bool = False,
                only_ip: str | None = None) -> dict[str, dict[str, Any]]:
    """Один адрес — одно плечо. tcp предпочитаем grpc: он же несёт long-poll.

    per_port: ключ «ip:port» вместо «ip». Нужен для каскадных подписок, где один
    входной адрес (72.56.246.204, РФ) раздаёт по портам РАЗНЫЕ зарубежные выходы:
    WB видит выход, а не вход, и схлопывание по входу проверяет один выход из 77."""
    best: dict[str, dict[str, Any]] = {}
    dead = 0
    for lane in lanes:
        ip = resolve(lane)
        if ip is None:
            dead += 1
            continue
        if only_ip and ip != only_ip:
            continue
        port = lane["settings"]["vnext"][0]["port"]
        if not skip_tcp and not tcp_alive(ip, port):
            dead += 1
            continue
        key = f"{ip}:{port}" if per_port else ip
        cur = best.get(key)
        if cur is None or (
            lane["streamSettings"]["network"] == "tcp"
            and cur["streamSettings"]["network"] != "tcp"
        ):
            best[key] = lane
    print(f"узлов {len(lanes)} | мёртвых/нерезолвнутых {dead} | уникальных живых адресов {len(best)}")
    return best


def one_lane_config(lane: dict[str, Any], port: int) -> dict[str, Any]:
    """Конфиг на одно плечо: без балансировщика первый outbound и есть маршрут."""
    return {
        "log": {"loglevel": "warning"},
        "inbounds": [{
            "tag": "in",
            "listen": "0.0.0.0",
            "port": port,
            "protocol": "http",
            "sniffing": {"enabled": True, "destOverride": ["http", "tls"]},
        }],
        "outbounds": [lane],
        "routing": {"domainStrategy": "AsIs", "rules": []},
    }


def test_lane(lane: dict[str, Any], ip: str, args) -> str:
    cfg_path = "/tmp/xray-one-lane.json"
    json.dump(one_lane_config(lane, args.port), open(cfg_path, "w"))
    sh("docker", "rm", "-f", args.name)
    up = sh("docker", "run", "-d", "--name", args.name, "--network", args.network,
            "-v", f"{cfg_path}:/etc/xray/config.json:ro", args.image)
    if up.returncode != 0:
        return "НЕ СТАРТОВАЛ: " + up.stderr.strip()[:60]
    try:
        time.sleep(args.warmup)
        probe = (PROBE_SRC
                 .replace("__PROXY__", f"http://{args.name}:{args.port}")
                 .replace("__URL__", args.url)
                 .replace("__N__", str(args.requests))
                 .replace("__DELAY__", str(args.delay)))
        out = sh("docker", "exec", "-i", args.probe_container, "python", "-c", probe)
        return out.stdout.strip() or out.stderr.strip()[:60] or "нет ответа"
    finally:
        sh("docker", "rm", "-f", args.name)


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group(required=True)
    src.add_argument("--sub", help="URL подписки: список узлов соберём генератором")
    src.add_argument("--lanes", help="готовый конфиг/список плеч (xray/config.json и т.п.)")
    ap.add_argument("--requests", type=int, default=3, help="запросов на плечо (по умолчанию 3)")
    ap.add_argument("--delay", type=float, default=0.0,
                    help="пауза между запросами одного плеча, сек. 0 = очередью подряд "
                         "(проверяет burst-лимит), 2-3 = размеренно (проверяет устойчивый лимит)")
    ap.add_argument("--url", default=WB_URL, help="какую ручку бить (по умолчанию боевая u-search)")
    ap.add_argument("--out", default="/tmp/wb-lane-results.json", help="куда сложить вердикты")
    ap.add_argument("--network", default="tryberrybot_default", help="docker-сеть compose")
    ap.add_argument("--probe-container", default="pt_wb_search_miner",
                    help="контейнер с python, из которого стучимся через временный xray")
    ap.add_argument("--image", default="teddysun/xray:latest")
    ap.add_argument("--name", default="xr_probe", help="имя временного контейнера")
    ap.add_argument("--port", type=int, default=8890)
    ap.add_argument("--warmup", type=float, default=3.0, help="сколько ждать старта xray, сек")
    ap.add_argument("--per-port", action="store_true",
                    help="не схлопывать порты одного адреса (каскадные подписки: выход у порта свой)")
    ap.add_argument("--only-ip", help="проверять только узлы с этим резолвнутым адресом")
    ap.add_argument("--skip-tcp", action="store_true", help="не отсеивать по TCP (быстрее, грязнее)")
    args = ap.parse_args()

    if args.sub:
        all_path = "/tmp/xray-all.json"
        gen = sh(sys.executable, GENERATOR, args.sub, "-n", "0", "-o", all_path)
        sys.stdout.write(gen.stdout)
        if gen.returncode != 0:
            sys.exit("генератор конфига упал: " + gen.stderr.strip()[:200])
        lanes_path = all_path
    else:
        lanes_path = args.lanes

    lanes = dedup_by_ip(load_lanes(lanes_path), args.skip_tcp, args.per_port, args.only_ip)
    if not lanes:
        sys.exit("живых плеч не осталось — нечего проверять")

    results: dict[str, dict[str, str]] = {}
    for i, (key, lane) in enumerate(sorted(lanes.items()), 1):
        ip = key.split(":")[0]
        verdict = test_lane(lane, ip, args)
        net = ".".join(ip.split(".")[:3]) + ".0/24"
        results[lane["tag"]] = {"ip": ip, "net": net, "verdict": verdict}
        print(f"[{i:2}/{len(lanes)}] {key:21} {lane['tag']:32} {verdict}", flush=True)

    json.dump(results, open(args.out, "w"), ensure_ascii=False, indent=1)

    clean = f"'200': {args.requests}"
    ok = [t for t, r in results.items() if clean in r["verdict"]]
    print(f"\nчистых плеч {len(ok)} из {len(results)} → {args.out}")

    by_net: dict[str, Counter] = defaultdict(Counter)
    for r in results.values():
        by_net[r["net"]]["ok" if clean in r["verdict"] else "bad"] += 1
    print("\nпо подсетям (чистых/всего):")
    for net, c in sorted(by_net.items(), key=lambda kv: (-kv[1]["ok"], kv[0])):
        print(f"  {net:20} {c['ok']}/{c['ok'] + c['bad']}")


if __name__ == "__main__":
    main()
