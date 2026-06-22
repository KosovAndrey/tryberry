#!/usr/bin/env python3
"""
ali_probe.py — Phase-1 спайк для aliexpress.ru (де-риск перед постройкой сайдкара).

По образцу ozon-miner/probe.py: поднимаем РЕАЛЬНЫЙ браузер (camoufox, headful в
Xvfb — headless палится антиботом) и открываем карточку товара. Живой браузер
должен прозрачно пройти антибот X5SEC (тот самый «punish» с x5secdata, об который
спотыкается голый HTTP даже с Chrome-JA3). Дальше снимаем цену из отрендеренного
DOM и/или из перехваченного API-ответа (mtop/aer-api) — это будущая цель парсера.

ЦЕЛЬ ЭКСПЕРИМЕНТА: проверить, проходит ли camoufox X5SEC с ЧИСТОГО датацентр-IP
VPS, БЕЗ прокси (ALI_PROXY пуст). Если да — строим сайдкар ali-miner по образцу
ozon-miner. Если нет — нужен резидентский/RU-прокси или vless.

Запуск — в образе ozon-miner (там camoufox+Xvfb+либы), см. run.sh / README.
"""
import contextlib
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

ALI_ID = os.getenv("ALI_ID", "1005005863682926").strip()
PROBE_URL = os.getenv("ALI_PROBE_URL", "").strip() or f"https://aliexpress.ru/item/{ALI_ID}.html"
PROXY_URL = os.getenv("ALI_PROXY", "").strip()  # по умолчанию БЕЗ прокси (датацентр-IP)
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
NAV_TIMEOUT_S = float(os.getenv("PROBE_NAV_TIMEOUT_SECONDS", "60"))
SETTLE_S = float(os.getenv("PROBE_SETTLE_SECONDS", "10"))  # JS: авто-челлендж + дозагрузка цены
OUT_DIR = os.getenv("OUT_DIR", "/tmp/aliprobe")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ali-probe")

# Подстроки URL потенциального API цены — их тела дампим как цель парсера.
API_HINTS = ("mtop", "aer-api", "/pdp", "pdp.pc", "/price", "/fn/", "detail", "acs.aliexpress")
# Маркеры антибота X5SEC.
PUNISH_MARKERS = ("x5secdata", "_____tmd_____", "/punish", "rgv587_flag", "punish?")


# ── Xvfb (как в ozon-miner/probe.py: проба сама поднимает дисплей) ────────────
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
    subprocess.Popen(
        ["Xvfb", disp, "-screen", "0", "1920x1080x24", "-nolisten", "tcp", "-ac"],
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
    server = f"{u.scheme}://{u.hostname}" + (f":{u.port}" if u.port else "")
    proxy = {"server": server}
    if u.username:
        proxy["username"] = unquote(u.username)
    if u.password:
        proxy["password"] = unquote(u.password)
    return proxy


@contextlib.contextmanager
def launch_browser():
    from camoufox.sync_api import Camoufox
    proxy = parse_proxy(PROXY_URL)
    kw = {"headless": HEADLESS, "geoip": bool(proxy), "humanize": True, "locale": "ru-RU"}
    if proxy:
        kw["proxy"] = proxy
    log.info("движок: camoufox | headless=%s | proxy=%s", HEADLESS, bool(proxy))
    with Camoufox(**kw) as browser:
        yield browser


def egress_ip(browser) -> str:
    try:
        p = browser.new_page()
        p.goto("https://api.ipify.org?format=json", timeout=15000)
        ip = json.loads(p.evaluate("() => document.body.innerText")).get("ip", "?")
        p.close()
        return ip
    except Exception:  # noqa: BLE001
        return "?"


# В отрендеренной странице ищем цену максимально устойчиво: og/meta, JSON-LD,
# глобальный стейт, видимый текст с рублём. Возвращаем найденные кандидаты.
_PRICE_JS = r"""
() => {
  const out = {};
  // 1) og:price / meta
  const meta = document.querySelector('meta[property="og:price:amount"], meta[itemprop="price"]');
  if (meta) out.meta = meta.content;
  // 2) JSON-LD Product
  for (const s of document.querySelectorAll('script[type="application/ld+json"]')) {
    try { const j = JSON.parse(s.textContent);
      const arr = Array.isArray(j) ? j : [j];
      for (const o of arr) if (o && o.offers && o.offers.price) out.jsonld = String(o.offers.price);
    } catch(e){}
  }
  // 3) видимый текст с рублём (первые 5 совпадений)
  const rub = (document.body.innerText.match(/[\d\s.,]+\s*₽/g) || []).slice(0,5);
  if (rub.length) out.visible_rub = rub;
  // 4) глобальные объекты с данными
  out.globals = Object.keys(window).filter(k => /run|init|state|data|pdp|sku|price/i.test(k)).slice(0,40);
  return out;
}
"""


def probe(browser) -> int:
    os.makedirs(OUT_DIR, exist_ok=True)
    page = browser.new_page()

    captured = []  # (url, status, response_obj)

    def on_response(resp):
        try:
            if any(h in resp.url for h in API_HINTS):
                captured.append((resp.url, resp.status, resp))
        except Exception:  # noqa: BLE001
            pass

    page.on("response", on_response)

    ip = egress_ip(browser)
    log.info("egress IP=%s | url=%s", ip, PROBE_URL)

    nav_status = None
    try:
        resp = page.goto(PROBE_URL, wait_until="domcontentloaded",
                         timeout=int(NAV_TIMEOUT_S * 1000))
        nav_status = resp.status if resp else None
    except Exception as e:  # noqa: BLE001
        log.warning("навигация: %s (продолжаю)", str(e).splitlines()[0])

    page.wait_for_timeout(int(SETTLE_S * 1000))

    content = page.content()
    final = page.url
    punished = any(m in content for m in PUNISH_MARKERS) or "/punish" in final

    # Если punish — даём браузеру шанс авто-решить и грузим карточку повторно.
    if punished:
        log.info("punish на первой загрузке → жду авто-решение и пробую снова")
        page.wait_for_timeout(int(SETTLE_S * 1000))
        with contextlib.suppress(Exception):
            page.goto(PROBE_URL, wait_until="domcontentloaded", timeout=int(NAV_TIMEOUT_S * 1000))
            page.wait_for_timeout(int(SETTLE_S * 1000))
        content = page.content()
        final = page.url
        punished = any(m in content for m in PUNISH_MARKERS) or "/punish" in final

    title = page.title()
    prices = {}
    with contextlib.suppress(Exception):
        prices = page.evaluate(_PRICE_JS)

    # Артефакты.
    html_path = os.path.join(OUT_DIR, "page.html")
    with open(html_path, "w", encoding="utf-8") as f:
        f.write(content)
    png_path = os.path.join(OUT_DIR, "page.png")
    with contextlib.suppress(Exception):
        page.screenshot(path=png_path, full_page=False)

    # Дамп перехваченных API-ответов с ценой.
    saved = []
    for i, (url, status, resp) in enumerate(captured, 1):
        body = ""
        with contextlib.suppress(Exception):
            body = resp.text()
        if ("price" in body.lower() or "pdp" in url) and len(body) > 50:
            fn = os.path.join(OUT_DIR, f"resp_{i}.json")
            with open(fn, "w", encoding="utf-8") as f:
                f.write(body)
            saved.append((url, status, len(body), fn))

    has_price = bool(prices.get("meta") or prices.get("jsonld") or prices.get("visible_rub")) or bool(saved)

    # ── Вердикт ──────────────────────────────────────────────────────────────
    print("\n================= ВЕРДИКТ =================")
    print(f"egress IP:      {ip}")
    print(f"nav status:     {nav_status}")
    print(f"final url:      {final}")
    print(f"title:          {title!r}")
    print(f"html size:      {len(content)} bytes")
    print(f"X5SEC punish:   {'ДА — НЕ пройден ❌' if punished else 'нет — пройден ✅'}")
    print(f"цена доступна:  {'ДА ✅' if has_price else 'нет ❌'}")
    print(f"цена-кандидаты: {json.dumps(prices.get('meta') or prices.get('jsonld') or prices.get('visible_rub'), ensure_ascii=False)}")
    print(f"window globals: {prices.get('globals')}")
    print("\n--- перехваченные API-ответы с ценой ---")
    if not saved:
        print("(не поймано; всего ответов по хинтам:", len(captured), ")")
        for url, status, _ in captured[:15]:
            print(f"  [{status}] {url[:120]}")
    for url, status, ln, fn in saved:
        print(f"  [{status}] len={ln} {url[:100]} → {fn}")
    print("\n--- артефакты ---")
    print(f"HTML:       {html_path}")
    print(f"screenshot: {png_path}")
    print(f"API-тела:   {OUT_DIR}/resp_*.json")
    print("==========================================")
    return 0 if (not punished and has_price) else 2


def main() -> int:
    ensure_display()
    with launch_browser() as browser:
        return probe(browser)


if __name__ == "__main__":
    sys.exit(main())
