#!/usr/bin/env python3
"""Автообновление плеч xray: подписки → проба → отбор → перекат при изменениях.

Зачем. Узлы у провайдеров осыпаются: за трое суток из 16 плеч конфига умерли 6, и
каждое мёртвое плечо в пуле с random стоит до 12с таймаута на запрос. Держать это
руками — писать одну и ту же цепочку команд раз в несколько дней.

Что делает по шагам:
  1. собирает узлы всех подписок из XRAY_SUBS (.env или окружение);
  2. отбирает N живых по кругу по /24 (xray-lanes-pick.py);
  3. с --verify дополнительно проверяет каждое плечо против ручки WB
     (wb-lane-test.py) и выкидывает те, что не отдали ни одной двухсотки —
     это ловит узлы с открытым портом, но неподнимающимся тоннелем;
  4. сравнивает НАБОР адресов с текущим конфигом. Совпал — выходит, ничего не
     трогая: перекат xray рвёт egress на несколько секунд, и делать это без
     причины незачем;
  5. изменился — бэкапит конфиг, пишет новый, `xray -test`, и только после
     успешной проверки пересоздаёт контейнер. Если тест упал, возвращает бэкап.

Примеры:
    python3 scripts/xray-lanes-auto.py --dry-run
    python3 scripts/xray-lanes-auto.py --verify
    python3 scripts/xray-lanes-auto.py --limit 24 --no-restart

Cron (раз в сутки в тихий час; PATH обязателен — в cron его нет):
    0 4 * * * PATH=/usr/local/bin:/usr/bin:/bin cd /home/USER/projects/tryberrybot \
      && /usr/bin/python3 scripts/xray-lanes-auto.py --verify >> /tmp/xray-lanes-auto.log 2>&1
"""

from __future__ import annotations

import argparse
import datetime
import glob
import json
import os
import shutil
import subprocess
import sys

HERE = os.path.dirname(os.path.abspath(__file__))
ROOT = os.path.dirname(HERE)
PICK = os.path.join(HERE, "xray-lanes-pick.py")
LANE_TEST = os.path.join(HERE, "wb-lane-test.py")
CANDIDATE = "/tmp/xray-lanes-candidate.json"
VERDICTS = "/tmp/xray-lanes-auto-verdicts.json"
KEEP_BACKUPS = 5


def log(msg: str) -> None:
    print(f"[{datetime.datetime.now():%F %T}] {msg}", flush=True)


def env_from_dotenv(path: str) -> dict[str, str]:
    """Читаем только нужные ключи: .env содержит секреты, в лог его не тащим."""
    out: dict[str, str] = {}
    if not os.path.exists(path):
        return out
    for line in open(path, encoding="utf-8", errors="replace"):
        line = line.strip()
        if not line or line.startswith("#") or "=" not in line:
            continue
        key, _, value = line.partition("=")
        if key.strip() in ("XRAY_SUBS", "XRAY_SUB_HWID"):
            out[key.strip()] = value.strip()
    return out


def lane_addrs(cfg_path: str) -> set[str]:
    """Набор адресов плеч — по нему и решаем, изменился ли пул."""
    if not os.path.exists(cfg_path):
        return set()
    cfg = json.load(open(cfg_path))
    out = set()
    for ob in cfg.get("outbounds", []):
        if not str(ob.get("tag", "")).startswith("vless"):
            continue
        v = (ob.get("settings") or {}).get("vnext") or [{}]
        if v[0].get("address"):
            out.add(f'{v[0]["address"]}:{v[0].get("port", 443)}')
    return out


def run(cmd: list[str], env: dict[str, str]) -> subprocess.CompletedProcess:
    return subprocess.run(cmd, text=True, capture_output=True, env=env)


def run_streaming(cmd: list[str], env: dict[str, str], prefix: str = "  ") -> tuple[int, str]:
    """Как run(), но вывод дочернего скрипта идёт наружу СРАЗУ.

    Шаги отбора и пробы занимают минуты; молчание всё это время неотличимо от
    зависания — и в терминале, и в cron-логе. Поэтому строки печатаем по мере
    поступления, а заодно копим для разбора итоговой строки.
    """
    proc = subprocess.Popen(cmd, text=True, env=env, bufsize=1,
                            stdout=subprocess.PIPE, stderr=subprocess.STDOUT)
    lines: list[str] = []
    assert proc.stdout is not None
    for line in proc.stdout:
        line = line.rstrip()
        lines.append(line)
        if line:
            print(prefix + line, flush=True)
    return proc.wait(), "\n".join(lines)


def summary_line(out: str, needle: str = "уникальных адресов") -> str:
    """Из болтливого вывода отбора берём одну содержательную строку для лога cron."""
    lines = [ln.strip() for ln in out.strip().splitlines() if ln.strip()]
    for ln in lines:
        if needle in ln:
            return ln
    return lines[-1] if lines else "выполнено"


def rotate_backups(cfg_path: str) -> None:
    backups = sorted(glob.glob(cfg_path + ".bak-*"))
    for old in backups[:-KEEP_BACKUPS]:
        try:
            os.remove(old)
        except OSError:
            pass


def main() -> int:
    ap = argparse.ArgumentParser(description=__doc__,
                                 formatter_class=argparse.RawDescriptionHelpFormatter)
    ap.add_argument("--config", default=os.path.join(ROOT, "xray", "config.json"))
    ap.add_argument("--env-file", default=os.path.join(ROOT, ".env"))
    ap.add_argument("--limit", type=int, default=40,
                    help="сколько плеч держать. Больше плеч = шире ротация: random на :8889 "
                         "выбирает плечо НА СОЕДИНЕНИЕ, а keep-alive выключен, так что каждый "
                         "запрос к WB уходит с нового адреса. Цена — observatory пробит каждое "
                         "плечо раз в 30с ради телеграмного leastPing")
    ap.add_argument("--verify", action="store_true",
                    help="проверить кандидатов против ручки WB и выкинуть безответные "
                         "(дольше: поднимает одноразовый xray на каждое плечо)")
    ap.add_argument("--min-lanes", type=int, default=5,
                    help="не перекатывать на пул меньше этого размера (по умолчанию 5)")
    ap.add_argument("--dry-run", action="store_true", help="показать решение, ничего не менять")
    ap.add_argument("--no-restart", action="store_true", help="записать конфиг, но не трогать контейнер")
    ap.add_argument("--container", default="pt_xray", help="имя контейнера xray (для xray -test)")
    ap.add_argument("--service", default="xray", help="имя сервиса в compose (для пересоздания)")
    ap.add_argument("--compose", default="docker compose -f docker-compose.yml -f docker-compose.prod.yml",
                    help="команда compose для пересоздания контейнера")
    args = ap.parse_args()

    env = dict(os.environ)
    env.update({k: v for k, v in env_from_dotenv(args.env_file).items() if k not in env})
    if not env.get("XRAY_SUBS"):
        log("XRAY_SUBS не задан ни в окружении, ни в .env — нечего обновлять")
        return 2

    subs = [u for u in env["XRAY_SUBS"].split(",") if u.strip()]
    log(f"подписок: {len(subs)}, целевой размер пула: {args.limit}")

    pick_cmd = [sys.executable, PICK, "-n", str(args.limit), "-o", CANDIDATE,
                "--prefer-current", args.config]
    log("шаг 1/3: тянем подписки и пробим узлы")
    code, out = run_streaming(pick_cmd, env)
    if code != 0:
        log("отбор плеч упал: " + out.strip()[-300:])
        return 1
    log(summary_line(out))

    if args.verify:
        # Проба ловит узлы, у которых порт открыт, а тоннель не встаёт: TCP-проба
        # их пропускает, а в бою каждый такой стоит таймаута.
        log("шаг 2/3: бьём каждое плечо в ручку WB (по 3 запроса, одноразовый xray на плечо)")
        code, out = run_streaming([sys.executable, LANE_TEST, "--lanes", CANDIDATE,
                                   "--out", VERDICTS], env)
        if code != 0:
            log("проба плеч упала, продолжаем без неё: " + out.strip()[-200:])
        else:
            log(summary_line(out, "чистых плеч"))
            log("шаг 3/3: пересобираем пул без плеч с неподнявшимся тоннелем")
            code, out = run_streaming(pick_cmd + ["--results", VERDICTS], env)
            if code != 0:
                log("повторный отбор упал: " + out.strip()[-200:])
                return 1
            log(summary_line(out))

    new, cur = lane_addrs(CANDIDATE), lane_addrs(args.config)
    if not new:
        log("кандидат пуст — конфиг не трогаем")
        return 1
    # Крошечный пул — одна точка отказа для всего TG-egress (17.09 перекатились на
    # одно плечо). Лучше оставить старый конфиг и звать человека.
    if len(new) < args.min_lanes:
        log(f"в кандидате {len(new)} плеч < --min-lanes {args.min_lanes} — конфиг не трогаем, "
            "разбираться руками")
        return 1
    added, gone = sorted(new - cur), sorted(cur - new)
    if not added and not gone:
        log(f"пул не изменился ({len(cur)} плеч) — xray не трогаем")
        return 0

    log(f"изменения: +{len(added)} / -{len(gone)}")
    for a in added:
        log(f"  + {a}")
    for g in gone:
        log(f"  - {g}")
    if args.dry_run:
        log("--dry-run: конфиг не записан")
        return 0

    backup = args.config + ".bak-" + datetime.datetime.now().strftime("%F-%H%M%S")
    if os.path.exists(args.config):
        shutil.copy(args.config, backup)
    shutil.copy(CANDIDATE, args.config)

    test = run(["docker", "exec", args.container, "xray", "-test", "-c", "/etc/xray/config.json"], env)
    if test.returncode != 0:
        # Конфиг мог оказаться битым — откатываемся немедленно, egress важнее
        # свежих плеч.
        if os.path.exists(backup):
            shutil.copy(backup, args.config)
        log("xray -test не прошёл, конфиг откачен: " + (test.stdout + test.stderr).strip()[-300:])
        return 1

    if args.no_restart:
        log("конфиг записан, перекат пропущен (--no-restart)")
        rotate_backups(args.config)
        return 0

    # Имя сервиса в compose (xray) и имя контейнера (pt_xray) различаются, поэтому
    # это два отдельных ключа, а не догадки по префиксу.
    up = run(args.compose.split() + ["up", "-d", "--force-recreate", args.service], env)
    if up.returncode != 0:
        log("перекат не удался: " + (up.stdout + up.stderr).strip()[-300:])
        return 1
    log(f"xray перекатан, плеч в пуле: {len(new)}")
    rotate_backups(args.config)
    return 0


if __name__ == "__main__":
    sys.exit(main())
