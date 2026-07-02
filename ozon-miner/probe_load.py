#!/usr/bin/env python3
"""
probe_load.py — СЕРИЙНЫЙ анонимный probe под нагрузкой (де-риск «жизни без прокси»).

Зачем: одиночный probe.py доказал, что camoufox проходит FAB анонимно даже с
датацентрового IP. Но мобильный прокси ценен именно под УСТОЙЧИВОЙ нагрузкой —
FAB может пустить холодный визит и зафлажить IP, когда с него польётся поток.
Этот скрипт эмулирует ОДНУ дорожку (одна camoufox-сессия) под боевым кадансом и
считает долю blocked/ok, чтобы решить: можно ли гнать Ozon без мобильного прокси.

Как читать: гоняй ДВАЖДЫ — раз с OZON_PROXY_URL="" (датацентр-IP), раз с мобильным
прокси. Сравни blocked_rate. Если без прокси долго держит ~0 блоков — мобильный
прокси для Ozon можно урезать (это ~90% прокси-бюджета).

⚠️ РИСК: серия анонимных запросов с боевого egress-IP может подпалить репутацию
этого IP у FAB. Гоняй с изолированного IP или будь готов переждать.

Движок — camoufox (как в проде). Headful в Xvfb (headless палится). Запуск в
образе ozon-miner. Команду см. в README.

ENV:
  OZON_PROXY_URL      — прокси (пусто = напрямую с датацентр-IP; это и есть тест)
  OZON_LOAD_IDS       — id товаров через запятую (ОБЯЗАТЕЛЬНО задать реальные из
                        своего трек-листа; дефолт — один тестовый, мало показателен)
  OZON_LOAD_N         — сколько запросов сделать всего (дефолт 60)
  OZON_LOAD_INTERVAL_S— средний интервал между запросами, сек (дефолт 20 = боевой пол)
  OZON_LOAD_JITTER    — джиттер интервала, доля (дефолт 0.4 = ±40%)
"""

import json
import logging
import os
import random
import re
import shutil
import subprocess
import sys
import time
from urllib.parse import unquote, urlparse

from camoufox.sync_api import Camoufox

PROXY_URL = os.getenv("OZON_PROXY_URL", "").strip()
WARM_ID = os.getenv("OZON_WARM_PRODUCT_ID", "1889984997")
IDS = [x.strip() for x in os.getenv("OZON_LOAD_IDS", WARM_ID).split(",") if x.strip()]
N = int(os.getenv("OZON_LOAD_N", "60"))
INTERVAL_S = float(os.getenv("OZON_LOAD_INTERVAL_S", "20"))
JITTER = float(os.getenv("OZON_LOAD_JITTER", "0.4"))
NAV_TIMEOUT_S = float(os.getenv("PROBE_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("PROBE_WARM_WAIT_SECONDS", "45"))
FETCH_TIMEOUT_S = float(os.getenv("PROBE_FETCH_TIMEOUT_SECONDS", "15"))
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")

logging.basicConfig(level=logging.INFO, format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-load")

# In-page fetch — тот же «метод друга», что в проде (server.py _FETCH_JS).
_FETCH_JS = """
async (id) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' +
              encodeURIComponent('/product/' + id + '/');
  try {
    const r = await fetch(url, {
      headers: {'accept': 'application/json', 'x-requested-with': 'XMLHttpRequest'},
      credentials: 'include',
    });
    const body = await r.text();
    return {status: r.status, widgets: body.includes('widgetStates'),
            ruble: body.includes('\\u20bd'),
            fab: body.includes('fab_') || body.includes('incidentId'),
            len: body.length};
  } catch (e) { return {status: -1, error: String(e)}; }
}
"""


def _display_alive(disp: str) -> bool:
    num = disp.lstrip(":").split(".")[0]
    return os.path.exists(f"/tmp/.X11-unix/X{num}")


def ensure_display():
    if HEADLESS:
        return
    disp = os.getenv("DISPLAY") or ":99"
    if _display_alive(disp):
        os.environ["DISPLAY"] = disp
        return
    if not shutil.which("Xvfb"):
        log.warning("Xvfb не найден, а HEADLESS=false — браузер может не стартовать")
        return
    subprocess.Popen(["Xvfb", disp, "-screen", "0", "1920x1080x24", "-nolisten", "tcp", "-ac"],
                     stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    os.environ["DISPLAY"] = disp
    for _ in range(25):
        if _display_alive(disp):
            break
        time.sleep(0.2)
    log.info("поднял Xvfb на %s", disp)


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


def egress_ip(browser) -> str:
    try:
        p = browser.new_page(no_viewport=True)
        p.goto("https://api.ipify.org?format=json", timeout=15000)
        ip = json.loads(p.evaluate("() => document.body.innerText")).get("ip", "?")
        p.close()
        return ip
    except Exception:  # noqa: BLE001
        return "?"


def _nudge(page):
    try:
        page.mouse.wheel(0, random.randint(200, 900))
    except Exception:  # noqa: BLE001
        pass


def fetch(page, pid: str):
    try:
        return page.evaluate(_FETCH_JS, pid) or {"status": -1}
    except Exception as e:  # noqa: BLE001
        return {"status": -1, "error": str(e).splitlines()[0]}


def classify(f: dict) -> str:
    st = f.get("status")
    if st == 200 and f.get("widgets") and not f.get("fab"):
        return "ok" if f.get("ruble") else "no_price"  # no_price = 18+ гейт/OOS
    if f.get("fab") or st == 403:
        return "blocked"
    return "error"


def main():
    ensure_display()
    proxy = parse_proxy(PROXY_URL)
    log.info("СЕРИЙНЫЙ probe: прокси=%s N=%d интервал=%.0fс±%.0f%% товаров=%d",
             proxy["server"] if proxy else "НЕТ (датацентр-direct)", N,
             INTERVAL_S, JITTER * 100, len(IDS))
    if IDS == [WARM_ID]:
        log.warning("OZON_LOAD_IDS не задан — гоняю по одному товару, мало показательно. "
                    "Задай реальные id из трек-листа через запятую.")

    # geoip=True — как в проде (server.py): выравнивает локаль/таймзону/гео под
    # exit-IP. БЕЗ него на RU-мобильном прокси FAB подозрительнее (locale-mismatch)
    # → дорожка не прогревается. Образ ozon-miner несёт camoufox[geoip].
    kw = {"headless": HEADLESS, "geoip": True}
    if proxy:
        kw["proxy"] = proxy
    tally = {"ok": 0, "no_price": 0, "blocked": 0, "error": 0}
    lat_sum, lat_n = 0.0, 0

    with Camoufox(**kw) as browser:
        ip = egress_ip(browser)
        log.info("exit-IP: %s", ip)
        page = browser.new_page(no_viewport=True)

        # Прогрев: навигация на карточку, ждём FAB-пропуск (in-page fetch 200).
        warm_url = f"https://www.ozon.ru/product/{WARM_ID}/"
        log.info("прогрев на %s …", warm_url)
        try:
            page.goto(warm_url, wait_until="domcontentloaded", timeout=int(NAV_TIMEOUT_S * 1000))
        except Exception as e:  # noqa: BLE001
            log.warning("навигация прогрева: %s", str(e).splitlines()[0])
        deadline = time.time() + WARM_WAIT_S
        warmed = False
        while time.time() < deadline:
            f = fetch(page, WARM_ID)
            if f.get("status") == 200 and not f.get("fab"):
                warmed = True
                break
            _nudge(page)
            page.wait_for_timeout(3000)
        if not warmed:
            log.error("НЕ прогрелся (FAB не пройден за %.0fс) — тест бессмыслен. "
                      "Вердикт: этот IP FAB не пускает даже холодным.", WARM_WAIT_S)
            sys.exit(2)
        log.info("прогрет, FAB пройден — начинаю серию из %d запросов", N)

        for k in range(1, N + 1):
            pid = random.choice(IDS)
            t0 = time.monotonic()
            f = fetch(page, pid)
            dt = time.monotonic() - t0
            cls = classify(f)
            tally[cls] += 1
            if cls in ("ok", "no_price"):
                lat_sum += dt
                lat_n += 1
            total = sum(tally.values())
            log.info("[%d/%d] id=%s → %s (status=%s, %.2fс, len=%s) | ok=%d no_price=%d "
                     "blocked=%d error=%d blocked_rate=%.1f%%",
                     k, N, pid, cls, f.get("status"), dt, f.get("len", "—"),
                     tally["ok"], tally["no_price"], tally["blocked"], tally["error"],
                     100.0 * tally["blocked"] / total)
            if k < N:
                wait = max(0.5, INTERVAL_S * (1.0 + JITTER * (2 * random.random() - 1)))
                time.sleep(wait)

    total = sum(tally.values()) or 1
    blocked_rate = 100.0 * tally["blocked"] / total
    print("\n" + "=" * 66)
    print(f"СЕРИЙНЫЙ PROBE — ИТОГ (exit-IP {ip}, прокси={'да' if proxy else 'НЕТ/датацентр'})")
    print("=" * 66)
    print(f"  запросов        : {total}")
    print(f"  ok (цена)       : {tally['ok']}")
    print(f"  no_price (18+/OOS): {tally['no_price']}")
    print(f"  blocked (FAB)   : {tally['blocked']}  → {blocked_rate:.1f}%")
    print(f"  error           : {tally['error']}")
    if lat_n:
        print(f"  avg latency ok  : {lat_sum / lat_n:.2f}с")
    print("=" * 66)
    if blocked_rate == 0:
        print("ВЕРДИКТ: ✅ ноль блоков под нагрузкой — этот IP держит поток. Если это "
              "датацентр-direct → мобильный прокси для Ozon можно урезать/убрать.")
    elif blocked_rate < 5:
        print("ВЕРДИКТ: ⚠️ редкие блоки — на грани. Прогони дольше/чаще, прежде чем решать.")
    else:
        print("ВЕРДИКТ: ❌ FAB флажит IP под нагрузкой — этому IP нужен мобильный прокси.")
    sys.exit(0)


if __name__ == "__main__":
    main()
