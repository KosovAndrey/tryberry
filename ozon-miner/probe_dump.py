#!/usr/bin/env python3
"""
probe_dump.py — снять СЫРОЙ widgetStates одного товара анонимно (для сверки
маркеров гейтов). Нужен, чтобы поймать живую сигнатуру 18+-гейта: какие виджеты
(ключи) и какие фразы приходят, когда цена спрятана за подтверждением возраста.
По ним чиним isOzonAgeGated в ozon.go, иначе гибрид не уйдёт на authed-дорожку.

Запуск (образ ozon-miner, там camoufox+Xvfb):
  docker run --rm -v "$PWD/ozon-miner:/probe" \
    -e OZON_PROXY_URL="" -e OZON_DUMP_ID=1729108994 \
    --entrypoint python tryberrybot-ozon-miner /probe/probe_dump.py
"""

import json
import logging
import os
import shutil
import subprocess
import sys
import time
from urllib.parse import unquote, urlparse

from camoufox.sync_api import Camoufox

PROXY_URL = os.getenv("OZON_PROXY_URL", "").strip()
WARM_ID = os.getenv("OZON_WARM_PRODUCT_ID", "1889984997")
DUMP_ID = os.getenv("OZON_DUMP_ID", "1729108994").strip()
NAV_TIMEOUT_S = float(os.getenv("PROBE_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("PROBE_WARM_WAIT_SECONDS", "45"))
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-dump")

# Возвращаем ПОЛНОЕ тело (не флаги) — его и разбираем на хосте.
_FETCH_JS = """
async (id) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' +
              encodeURIComponent('/product/' + id + '/');
  try {
    const r = await fetch(url, {headers: {'accept': 'application/json',
      'x-requested-with': 'XMLHttpRequest'}, credentials: 'include'});
    return {status: r.status, body: await r.text()};
  } catch (e) { return {status: -1, body: '', error: String(e)}; }
}
"""

# Подстроки-кандидаты в маркеры 18+-гейта (ищем и в ключах, и в значениях).
NEEDLES = ["adult", "age", "18", "возраст", "birth", "рожден", "рожд",
           "подтверд", "старше", "verif", "restrict"]


def ensure_display():
    if HEADLESS:
        return
    disp = os.getenv("DISPLAY") or ":99"
    num = disp.lstrip(":").split(".")[0]
    if os.path.exists(f"/tmp/.X11-unix/X{num}"):
        os.environ["DISPLAY"] = disp
        return
    if not shutil.which("Xvfb"):
        return
    subprocess.Popen(["Xvfb", disp, "-screen", "0", "1920x1080x24", "-nolisten", "tcp", "-ac"],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.environ["DISPLAY"] = disp
    for _ in range(25):
        if os.path.exists(f"/tmp/.X11-unix/X{num}"):
            break
        time.sleep(0.2)


def parse_proxy(url: str):
    if not url:
        return None
    u = urlparse(url)
    proxy = {"server": f"{u.scheme}://{u.hostname}" + (f":{u.port}" if u.port else "")}
    if u.username:
        proxy["username"] = unquote(u.username)
    if u.password:
        proxy["password"] = unquote(u.password)
    return proxy


def fetch(page, pid):
    try:
        return page.evaluate(_FETCH_JS, pid) or {"status": -1}
    except Exception as e:  # noqa: BLE001
        return {"status": -1, "error": str(e).splitlines()[0]}


def main():
    ensure_display()
    proxy = parse_proxy(PROXY_URL)
    kw = {"headless": HEADLESS, "geoip": True}
    if proxy:
        kw["proxy"] = proxy
    log.info("dump: прокси=%s warm=%s dump_id=%s",
             proxy["server"] if proxy else "НЕТ", WARM_ID, DUMP_ID)

    with Camoufox(**kw) as browser:
        page = browser.new_page(no_viewport=True)
        try:
            page.goto(f"https://www.ozon.ru/product/{WARM_ID}/",
                      wait_until="domcontentloaded", timeout=int(NAV_TIMEOUT_S * 1000))
        except Exception as e:  # noqa: BLE001
            log.warning("навигация прогрева: %s", str(e).splitlines()[0])
        deadline = time.time() + WARM_WAIT_S
        while time.time() < deadline:
            f = fetch(page, WARM_ID)
            if f.get("status") == 200 and "fab_" not in f.get("body", ""):
                break
            page.wait_for_timeout(3000)
        log.info("прогрет — тяну dump_id=%s", DUMP_ID)
        f = fetch(page, DUMP_ID)

    body = f.get("body", "")
    print("\n" + "=" * 66)
    print(f"DUMP id={DUMP_ID}  status={f.get('status')}  len={len(body)}")
    print("=" * 66)
    try:
        env = json.loads(body)
    except Exception as e:  # noqa: BLE001
        print(f"НЕ JSON: {e}\n--- первые 2000 символов ---\n{body[:2000]}")
        sys.exit(1)

    ws = env.get("widgetStates") or {}
    print(f"widgetStates: {len(ws)} виджетов")
    print("\n--- ключи виджетов ---")
    for k in sorted(ws):
        print(f"  {k}")

    print("\n--- совпадения по кандидатам-маркерам (ключ / значение) ---")
    hits = 0
    for k, v in ws.items():
        lk, lv = k.lower(), (v or "").lower()
        kmatch = [n for n in NEEDLES if n in lk]
        vmatch = [n for n in NEEDLES if n in lv]
        if kmatch or vmatch:
            hits += 1
            print(f"  [{k}]")
            if kmatch:
                print(f"    в КЛЮЧЕ: {kmatch}")
            if vmatch:
                print(f"    в значении: {vmatch}")
                print(f"    фрагмент: {(v or '')[:300]}")
    if not hits:
        print("  совпадений НЕТ — маркеры не подходят, смотри полный дамп ниже")
        print("\n--- полное тело (первые 4000 символов) ---")
        print(body[:4000])

    # Прочие поля конверта верхнего уровня (иногда гейт торчит вне widgetStates).
    print("\n--- прочие ключи конверта ---")
    for k in env:
        if k != "widgetStates":
            sv = json.dumps(env[k], ensure_ascii=False)
            print(f"  {k}: {sv[:200]}")


if __name__ == "__main__":
    main()
