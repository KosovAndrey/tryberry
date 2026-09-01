#!/usr/bin/env python3
"""Отбор плеч xray: живые, по одному на адрес, с максимальным разбросом по /24.

Зачем. Поисковая ручка WB лимитирует ПО АДРЕСУ выхода, поэтому потолок поиска
упирается не в скорость, а в число разных IP в пуле. Генератор
`xray-config-from-sub.py` берёт первые N узлов подписки как есть, а там
вперемешку мёртвые узлы и дубли: один адрес на пяти портах — для WB это ОДИН
адрес. В ночь на 01-09 из 16 плеч конфига живыми были 10, а уникальных адресов в
подписке нашлось 35 в 17 подсетях.

Подписок может быть НЕСКОЛЬКО: их адреса складываются в общий пул (лимит WB
считается по IP, поэтому чем больше разных адресов, тем лучше). Список живёт в
XRAY_SUBS в .env — добавить провайдера значит дописать туда ещё один URL.

Что делает: берёт полные конфиги (или собирает их из подписок), отсеивает
мёртвые узлы TCP-пробой, схлопывает по резолвнутому IP, затем выбирает N плеч
ПО КРУГУ ПО ПОДСЕТЯМ — сначала по одному из каждой /24, потом второй круг и так
далее. Так пул не вырождается в одну сеть, даже если в ней больше всего узлов.

Примеры:
    python3 scripts/xray-lanes-pick.py --from /tmp/xray-all.json -n 24
    python3 scripts/xray-lanes-pick.py --from /tmp/xray-all.json /tmp/xray-new.json -n 24
    python3 scripts/xray-lanes-pick.py --sub 'https://ПОДПИСКА-1' 'https://ПОДПИСКА-2' -n 24
    XRAY_SUBS='https://ПОДПИСКА-1,https://ПОДПИСКА-2' python3 scripts/xray-lanes-pick.py -n 24
    python3 scripts/xray-lanes-pick.py --from /tmp/xray-all.json \\
        --results /tmp/wb-lane-results.json      # выкинуть плечи, забракованные WB

После записи конфига: `docker exec pt_xray xray -test -c /etc/xray/config.json`
и `up -d --force-recreate xray` (несколько секунд без egress — бот молчит).
"""

from __future__ import annotations

import argparse
import concurrent.futures as cf
import datetime
import json
import os
import shutil
import socket
import subprocess
import sys
from collections import defaultdict
from typing import Any

HERE = os.path.dirname(os.path.abspath(__file__))
GENERATOR = os.path.join(HERE, "xray-config-from-sub.py")


def resolve(lane: dict[str, Any]) -> str | None:
    try:
        return socket.gethostbyname(lane["settings"]["vnext"][0]["address"])
    except OSError:
        return None


def tcp_alive(ip: str, port: int, timeout: float) -> bool:
    s = socket.socket()
    s.settimeout(timeout)
    try:
        s.connect((ip, port))
        return True
    except OSError:
        return False
    finally:
        s.close()


def subnet(ip: str) -> str:
    return ".".join(ip.split(".")[:3]) + ".0/24"


def pick_round_robin(by_net: dict[str, list], limit: int) -> list:
    """По кругу: сначала по одному плечу из каждой подсети, потом второй круг."""
    picked: list = []
    round_no = 0
    while len(picked) < limit:
        added = 0
        for net in sorted(by_net):
            if round_no < len(by_net[net]):
                picked.append(by_net[net][round_no])
                added += 1
                if len(picked) >= limit:
                    break
        if added == 0:
            break
        round_no += 1
    return picked


def main() -> None:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    src = ap.add_mutually_exclusive_group()
    src.add_argument("--sub", nargs="+", metavar="URL",
                     help="URL подписок (можно несколько). Если не задано — берём XRAY_SUBS "
                          "из окружения (список через запятую)")
    src.add_argument("--from", dest="src_paths", nargs="+",
                     help="готовые конфиги со всеми узлами; можно НЕСКОЛЬКО — "
                          "пулы разных подписок сливаются в один (теги при совпадении разводятся)")
    ap.add_argument("-n", "--limit", type=int, default=40,
                    help="сколько плеч оставить (по умолчанию 40). Ротация идёт на уровне "
                         "СОЕДИНЕНИЙ: random на :8889 берёт случайное плечо на каждый запрос, "
                         "поэтому широкий пул лучше узкого. Потолок нужен из-за observatory — "
                         "он пробит каждое плечо раз в 30с")
    ap.add_argument("-o", "--out", default="xray/config.json", help="куда писать конфиг")
    ap.add_argument("--results", help="JSON от wb-lane-test.py: выкинуть плечи, не давшие НИ ОДНОГО "
                                       "HTTP-ответа (не встал тоннель). 429 = плечо живое, не бракуем")
    ap.add_argument("--prefer-current", metavar="CONFIG",
                    help="конфиг с действующим пулом: живые плечи оттуда остаются на месте. "
                         "Без этого отбор каждый раз тасует пул заново, и ночной прогон "
                         "перекатывает xray (обрыв egress) без реальных изменений")
    ap.add_argument("--timeout", type=float, default=5.0, help="таймаут TCP-пробы, сек")
    ap.add_argument("--dry-run", action="store_true", help="показать отбор, ничего не записывать")
    args = ap.parse_args()

    subs = args.sub or [u.strip() for u in os.getenv("XRAY_SUBS", "").split(",") if u.strip()]
    if subs and args.src_paths:
        sys.exit("--from и подписки одновременно не имеют смысла: выбери что-то одно")
    if subs:
        # Каждая подписка разворачивается в свой файл: так видно, что откуда, и
        # можно переиспользовать выгрузку, не дёргая панель лишний раз (у панелей
        # с HWID-привязкой каждый запрос — это обращение за слотом устройства).
        src_paths = []
        for i, sub in enumerate(subs, 1):
            path = f"/tmp/xray-sub-{i}.json"
            gen = subprocess.run([sys.executable, GENERATOR, sub, "-n", "0", "-o", path],
                                 capture_output=True, text=True)
            if gen.returncode != 0:
                sys.exit(f"подписка №{i}: генератор упал: {gen.stderr.strip()[:200]}")
            sys.stderr.write(gen.stderr)
            src_paths.append(path)
    elif args.src_paths:
        src_paths = args.src_paths
    else:
        sys.exit("нечего собирать: задай --sub, --from или XRAY_SUBS в окружении")

    # Первый файл задаёт скелет (inbounds/routing/observatory), плечи берём из всех.
    cfg = json.load(open(src_paths[0]))
    tail = [o for o in cfg["outbounds"] if not str(o.get("tag", "")).startswith("vless")]
    lanes: list[dict[str, Any]] = []
    seen_tags: set[str] = set()
    for path in src_paths:
        one = json.load(open(path))
        for lane in one["outbounds"]:
            tag = str(lane.get("tag", ""))
            if not tag.startswith("vless"):
                continue
            # Слаги генерятся внутри каждой подписки, между подписками совпадают.
            if tag in seen_tags:
                n = 2
                while f"{tag}-s{n}" in seen_tags:
                    n += 1
                lane = dict(lane, tag=f"{tag}-s{n}")
            seen_tags.add(lane["tag"])
            lanes.append(lane)
    if len(src_paths) > 1:
        print(f"слито подписок: {len(src_paths)}, плеч всего {len(lanes)}")

    rejected: set[str] = set()
    if args.results:
        for tag, row in json.load(open(args.results)).items():
            # Бракуем ТОЛЬКО плечи без HTTP-ответа вообще: у них не встаёт тоннель
            # (TCP-проба это пропускает — порт открыт, а VLESS/Reality не поднялся).
            # 429 — признак ЗДОРОВОГО плеча: запрос дошёл до WB и та его увидела,
            # просто лимитирует; отбраковывать по нему нельзя, иначе ночной cron
            # будет тасовать исправный пул по настроению маркетплейса.
            verdict = row.get("verdict", "")
            if "'200'" not in verdict and "429" not in verdict:
                rejected.add(tag)

    # Пробим параллельно: последовательно 100 узлов с таймаутом 5с — это минуты
    # тишины, и в cron-логе выглядит как зависание.
    candidates = [ln for ln in lanes if ln["tag"] not in rejected]
    print(f"пробим {len(candidates)} узлов (TCP, таймаут {args.timeout:g}с)…", file=sys.stderr, flush=True)

    def probe(lane: dict[str, Any]) -> tuple[dict[str, Any], str | None]:
        ip = resolve(lane)
        if ip is None or not tcp_alive(ip, lane["settings"]["vnext"][0]["port"], args.timeout):
            return lane, None
        return lane, ip

    best: dict[str, dict[str, Any]] = {}
    dead = 0
    with cf.ThreadPoolExecutor(max_workers=32) as ex:
        for done, (lane, ip) in enumerate(ex.map(probe, candidates), 1):
            if done % 25 == 0 or done == len(candidates):
                print(f"  пробито {done}/{len(candidates)}", file=sys.stderr, flush=True)
            if ip is None:
                dead += 1
                continue
            cur = best.get(ip)
            # tcp предпочитаем grpc: тот же пул несёт long-poll Telegram.
            if cur is None or (lane["streamSettings"]["network"] == "tcp"
                               and cur["streamSettings"]["network"] != "tcp"):
                best[ip] = lane

    # Стабильность важнее «свежести»: плечо, которое уже работает, менять незачем.
    # Внутри подсети действующие идут первыми, остальные — по адресу (детерминизм).
    current: set[str] = set()
    if args.prefer_current:
        try:
            cur_cfg = json.load(open(args.prefer_current))
            for ob in cur_cfg.get("outbounds", []):
                v = (ob.get("settings") or {}).get("vnext") or [{}]
                if v[0].get("address"):
                    current.add(str(v[0]["address"]))
        except (OSError, ValueError):
            pass  # нет конфига или битый — просто отбираем без предпочтений

    def stability_key(item: tuple[str, dict[str, Any]]) -> tuple[int, str]:
        ip, lane = item
        host = str(lane["settings"]["vnext"][0]["address"])
        return (0 if (ip in current or host in current) else 1, ip)

    by_net: dict[str, list] = defaultdict(list)
    for ip, lane in sorted(best.items(), key=stability_key):
        by_net[subnet(ip)].append((ip, lane))
    if current:
        kept = sum(1 for ip, lane in best.items()
                   if ip in current or str(lane["settings"]["vnext"][0]["address"]) in current)
        print(f"действующих плеч живо: {kept} из {len(current)}", file=sys.stderr, flush=True)

    picked = pick_round_robin(by_net, args.limit)
    print(f"узлов {len(lanes)} | забраковано WB {len(rejected)} | мёртвых {dead} | "
          f"уникальных адресов {len(best)} в {len(by_net)} подсетях | берём {len(picked)}")
    for net in sorted({subnet(ip) for ip, _ in picked}):
        tags = [f"{ip} {lane['tag']}" for ip, lane in picked if subnet(ip) == net]
        print(f"  {net:20} {len(tags)}  " + ", ".join(t.split()[1] for t in tags))

    if args.dry_run:
        print("\n--dry-run: конфиг не записан")
        return

    cfg["outbounds"] = [lane for _, lane in picked] + tail
    if os.path.exists(args.out):
        backup = args.out + ".bak-" + datetime.datetime.now().strftime("%F-%H%M")
        shutil.copy(args.out, backup)
        print(f"\nбэкап прежнего конфига: {backup}")
    json.dump(cfg, open(args.out, "w"), ensure_ascii=False, indent=2)
    print(f"записан {args.out}: плеч {len(picked)}")
    print("дальше: docker exec pt_xray xray -test -c /etc/xray/config.json && "
          "docker compose -f docker-compose.yml -f docker-compose.prod.yml up -d --force-recreate xray")


if __name__ == "__main__":
    main()
