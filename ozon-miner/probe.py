#!/usr/bin/env python3
"""
probe.py — спайк «метода друга» для Ozon (де-риск перед постройкой пула дорожек).

МЕТОД ДРУГА (что именно проверяем):
  1) поднять РЕАЛЬНЫЙ Chromium (Patchright, стелс-форк) через прод-прокси;
  2) залогинить его, вложив аккаунт-cookie (OZON_COOKIE) ДО навигации;
  3) открыть страницу на www.ozon.ru — живая доверенная сессия проходит FAB;
  4) запросить карточку API-запросом ИЗНУТРИ этой же страницы (in-page fetch):
     same-origin, с её куками и её JA3, а решённый FAB-челлендж — общий.
  Куку наружу НЕ отдаём и Go-tls-client НЕ реплеим — в этом вся соль: транспорт и
  доверие FAB остаются согласованными.

Проба ничего не майнит и не пишет — открывает один товар и печатает вердикт с
кодом возврата (см. таблицу внизу). Это решает «да/нет» перед фазой 2 (пул).

Самодостаточна: сама поднимает Xvfb, если дисплея нет. Запуск в готовом образе
(там уже Patchright+браузеры), БЕЗ сборки нового — команда в README.
"""

import json
import logging
import os
import re
import shutil
import subprocess
import sys
import time
from urllib.parse import unquote, urlparse

from patchright.sync_api import sync_playwright
from patchright.sync_api import TimeoutError as PWTimeout

# ── Конфиг ───────────────────────────────────────────────────────────────────
# Реальный товар по умолчанию (не 18+, цена видна) — из боевых логов 15.06.
DEFAULT_URL = ("https://www.ozon.ru/product/"
               "mixit-patch-ot-pryshchey-gelevyy-s-kislotami-dlya-problemnoy-"
               "kozhi-stop-acne-15-ml-1889984997/")

PROBE_URL = os.getenv("OZON_PROBE_URL", "").strip() or DEFAULT_URL
PROXY_URL = os.getenv("OZON_MINER_PROXY_URL", os.getenv("OZON_PROXY_URL", "")).strip()
OZON_COOKIE = os.getenv("OZON_COOKIE", "").strip()
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
LOCALE = os.getenv("MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("MINER_TIMEZONE", "Europe/Moscow")
NAV_TIMEOUT_S = float(os.getenv("PROBE_NAV_TIMEOUT_SECONDS", "45"))
SETTLE_S = float(os.getenv("PROBE_SETTLE_SECONDS", "4"))
# Сколько ждём авто-решения challenge.html (JS-VM исполняется в браузере и
# редиректит обратно). Повторяем in-page fetch, пока не 200 или не истечёт.
CHALLENGE_WAIT_S = float(os.getenv("PROBE_CHALLENGE_WAIT_SECONDS", "40"))
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-probe")

_ID_RE = re.compile(r"/product/(?:[^/?#]*-)?(\d+)")
_INCIDENT_RE = re.compile(r'"incidentId":\s*"(fab_[A-Za-z0-9_]+)"')

# In-page fetch — сердце метода друга. credentials:'include' тащит куки сессии;
# запрос едет тем же JA3, что и прошедший FAB браузер. Возвращаем сырые признаки.
_FRIEND_FETCH_JS = """
async (id) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' +
              encodeURIComponent('/product/' + id + '/');
  try {
    const r = await fetch(url, {
      headers: {'accept': 'application/json', 'x-requested-with': 'XMLHttpRequest'},
      credentials: 'include',
    });
    const body = await r.text();
    return {
      status: r.status,
      widgets: body.includes('widgetStates'),
      ruble:   body.includes('\\u20bd'),
      fab:     body.includes('fab_') || body.includes('incidentId'),
      len:     body.length,
      snippet: body.slice(0, 240),
    };
  } catch (e) { return {status: -1, error: String(e)}; }
}
"""


def _display_alive(disp: str) -> bool:
    """Жив ли X-сервер на дисплее: проверяем unix-сокет /tmp/.X11-unix/X<n>.
    Переменной DISPLAY недостаточно — образ может её выставлять без живого Xvfb."""
    num = disp.lstrip(":").split(".")[0]
    return os.path.exists(f"/tmp/.X11-unix/X{num}")


def ensure_display():
    """Поднять Xvfb, если headful и живого дисплея ещё нет. Делает команду
    запуска тривиальной (не нужно городить Xvfb в docker-run)."""
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
    # ждём появления сокета (до ~5с), не фиксированный sleep
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


def cookie_jar(header: str):
    """'k=v; k2=v2' → формат Playwright add_cookies на домен .ozon.ru
    (покрывает www. и api. поддомены). secure=True ОБЯЗАТЕЛЕН: иначе Chrome
    отвергает всю пачку из-за куки с префиксом __Secure-/__Host-."""
    out = []
    for part in header.split(";"):
        if "=" not in part:
            continue
        k, v = part.strip().split("=", 1)
        if k.strip():
            out.append({"name": k.strip(), "value": v.strip(),
                        "domain": ".ozon.ru", "path": "/", "secure": True})
    return out


def add_cookies_safe(context, cookies) -> int:
    """Добавить куки устойчиво: сперва пачкой, при отказе — по-одной, пропуская
    кривые (в строке из приложения бывают не-cookie поля вроде x-o3-* со скобками)."""
    try:
        context.add_cookies(cookies)
        return len(cookies)
    except Exception:  # noqa: BLE001
        ok = 0
        for c in cookies:
            try:
                context.add_cookies([c])
                ok += 1
            except Exception as e:  # noqa: BLE001
                log.warning("пропускаю cookie %r: %s", c["name"], str(e).splitlines()[0])
        return ok


def product_id(url: str) -> str:
    m = _ID_RE.search(url)
    return m.group(1) if m else ""


def _nudge(page):
    """Лёгкая имитация живого юзера — помогает challenge.html досчитаться."""
    import random
    try:
        page.mouse.move(random.randint(80, 1200), random.randint(80, 680),
                        steps=random.randint(4, 9))
        page.mouse.wheel(0, random.randint(200, 900))
    except Exception:  # noqa: BLE001
        pass


def egress_ip(context) -> str:
    try:
        p = context.new_page()
        p.goto("https://api.ipify.org?format=json", timeout=15000)
        ip = json.loads(p.evaluate("() => document.body.innerText")).get("ip", "?")
        p.close()
        return ip
    except Exception:  # noqa: BLE001
        return "?"


def probe(pw) -> int:
    pid = product_id(PROBE_URL)
    if not pid:
        log.error("не разобрал id товара из URL: %s", PROBE_URL)
        return 1

    launch = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
    proxy = parse_proxy(PROXY_URL)
    if proxy:
        launch["proxy"] = proxy

    browser = pw.chromium.launch(**launch)
    try:
        context = browser.new_context(locale=LOCALE, timezone_id=TIMEZONE,
                                      viewport={"width": 1366, "height": 768})

        injected = cookie_jar(OZON_COOKIE)
        logged_in = any(c["name"] == "__Secure-access-token" for c in injected)
        added = add_cookies_safe(context, injected) if injected else 0
        log.info("прокси=%s | cookie=%d/%d шт (logged_in=%s) | url=%s",
                 proxy["server"] if proxy else "НЕТ", added, len(injected), logged_in, PROBE_URL)
        if not logged_in:
            log.warning("в OZON_COOKIE нет __Secure-access-token — это НЕ метод друга "
                        "(аноним FAB режет). Подставь cookie залогиненного аккаунта.")

        ip = egress_ip(context)

        # Навигация на карточку: живая сессия проходит FAB и задаёт origin ozon.ru,
        # чтобы in-page fetch был same-origin и нёс куки.
        page = context.new_page()
        nav_status = None
        try:
            resp = page.goto(PROBE_URL, wait_until="domcontentloaded",
                             timeout=int(NAV_TIMEOUT_S * 1000))
            nav_status = resp.status if resp else None
        except PWTimeout:
            log.warning("навигация: таймаут (продолжаю — fetch может сработать)")
        except Exception as e:  # noqa: BLE001
            log.warning("навигация упала: %s", e)
        page.wait_for_timeout(int(SETTLE_S * 1000))

        # ── Метод друга: in-page fetch с ОЖИДАНИЕМ авто-решения челленджа ─────
        # FAB отдаёт challenge.html (JS-VM) — даём браузеру время её исполнить и
        # стать доверенным, повторяя fetch, пока не 200 (или таймаут).
        f = {"status": -1}
        deadline = time.time() + CHALLENGE_WAIT_S
        attempt = 0
        while time.time() < deadline:
            attempt += 1
            try:
                f = page.evaluate(_FRIEND_FETCH_JS, pid) or {}
            except Exception as e:  # noqa: BLE001
                f = {"status": -1, "error": str(e)}
            if f.get("status") == 200 and not f.get("fab"):
                break
            log.info("попытка %d: fetch status=%s fab=%s — жду решения челленджа",
                     attempt, f.get("status"), f.get("fab"))
            _nudge(page)
            page.wait_for_timeout(3000)

        # Признаки/диагностика.
        try:
            page_text = page.evaluate("() => document.body ? document.body.innerText : ''") or ""
        except Exception:  # noqa: BLE001
            page_text = ""
        price_on_page = "₽" in page_text
        try:
            final_url = page.url
            final_title = page.title()
        except Exception:  # noqa: BLE001
            final_url, final_title = "?", "?"
        on_challenge = "challenge" in (final_url + " " + final_title).lower()
        m_inc = _INCIDENT_RE.search(f.get("snippet", "") or "")
        incident = m_inc.group(1) if m_inc else ""
        inc_type = ("nmk" if "nmk" in incident else
                    "chlg" if "chlg" in incident else
                    "challenge" if incident else "")

        # ── Отчёт ────────────────────────────────────────────────────────────
        print("\n" + "=" * 66)
        print("OZON PROBE — МЕТОД ДРУГА")
        print("=" * 66)
        print(f"  exit-IP прокси     : {ip}")
        print(f"  залогинен (cookie)  : {'ДА' if logged_in else 'НЕТ (аноним)'}")
        print(f"  навигация status    : {nav_status if nav_status is not None else '—'}")
        print(f"  цена ₽ на странице  : {'ДА' if price_on_page else 'нет'}")
        print(f"  in-page fetch       : status={f.get('status')} "
              f"widgets={f.get('widgets')} ₽={f.get('ruble')} "
              f"fab={f.get('fab')} len={f.get('len', '—')}")
        if incident:
            print(f"  FAB-инцидент        : {incident} (тип: {inc_type})")
        print(f"  финальный URL       : {final_url}")
        print(f"  заголовок страницы  : {final_title!r}")
        print(f"  висим на челлендже  : {'ДА ⚠️' if on_challenge else 'нет'}")
        if f.get("error"):
            print(f"  fetch error         : {f['error']}")
        elif f.get("status") != 200:
            print(f"  fetch snippet       : {f.get('snippet', '')!r}")
        print("=" * 66)

        friend_ok = (f.get("status") == 200 and f.get("widgets")
                     and f.get("ruble") and not f.get("fab"))
        if friend_ok:
            print("ВЕРДИКТ: ✅✅ МЕТОД ДРУГА РАБОТАЕТ — из одного залогиненного браузера "
                  "in-page fetch отдаёт widgetStates+цену. Строим пул дорожек "
                  "(server.py), Go ходит в browser-режиме.")
            return 0
        if inc_type == "nmk":
            print("ВЕРДИКТ: ❌ FAB-инцидент nmk = жёсткий бан egress (датацентр/спалённый IP). "
                  "Сменить мобильный IP ссылкой-ротацией и повторить.")
            return 2
        if not logged_in:
            print("ВЕРДИКТ: ⚠️ Заход анонимный → это не метод друга. Подставь OZON_COOKIE "
                  "залогиненного аккаунта и повтори.")
            return 3
        print("ВЕРДИКТ: ❌ Залогиненный браузер упёрся в FAB-челлендж. Причины: либо "
              "FAB-доверие OZON_COOKIE протухло (снять свежую из приложения), либо "
              "Chromium-в-Xvfb палится фингерпринтом (нужен сильнее анти-детект: "
              "реальный Chrome-канал / camoufox). Тогда — fallback на платный API.")
        return 4
    finally:
        try:
            browser.close()
        except Exception:  # noqa: BLE001
            pass


# Коды возврата: 0 метод друга ОК (строим пул) | 2 nmk-бан IP (сменить IP) |
# 3 аноним (подставить cookie) | 4 залогинен, но FAB (свежая cookie / анти-детект).
def main():
    log.info("OZON PROBE старт: headless=%s", HEADLESS)
    ensure_display()
    with sync_playwright() as pw:
        sys.exit(probe(pw))


if __name__ == "__main__":
    main()
