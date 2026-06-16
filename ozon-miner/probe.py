#!/usr/bin/env python3
"""
probe.py — спайк-проба гипотезы Ozon-майнера (фаза 1, де-риск перед пулом).

Открывает карточку товара Ozon в реальном Chromium (Patchright, стелс-форк
Playwright) через ТОТ ЖЕ прокси, что у скрейпера, и отвечает на три вопроса,
от которых зависит весь дизайн майнера:

  1) Проходит ли АНОНИМНЫЙ браузер FAB через прод-прокси? (видим 200 на
     entrypoint-api без fab_/403 → да)
  2) Видна ли цена без логина? (₽ в ответе/на странице → логин не нужен,
     майнер можно делать без credentials и SMS-кода)
  3) Какие куки выдаёт сессия? (есть ли trust-набор abt_data/__Secure-* —
     именно его реплеит Go-скрейпер)

Не пишет в Redis, ничего не майнит — только смотрит и печатает вердикт.
Запускать в уже собранном образе pt_token_miner (там Patchright+браузеры+Xvfb):

  docker run --rm --network tryberrybot_default \
    -v "$PWD/ozon-miner/probe.py:/app/probe.py" \
    -e OZON_PROBE_URL="https://www.ozon.ru/product/...-1889984997/" \
    -e OZON_MINER_PROXY_URL="$OZON_PROXY_URL" \
    -e HEADLESS=false \
    --entrypoint sh pt_token_miner \
    -c 'Xvfb :99 -screen 0 1920x1080x24 -nolisten tcp -ac >/tmp/x.log 2>&1 & \
        sleep 2; DISPLAY=:99 python /app/probe.py'

(OZON_MINER_PROXY_URL пусто → ходим напрямую, для сравнения «прокси vs без».)
"""

import logging
import os
import random
import re
import sys
import time
from collections import Counter
from urllib.parse import unquote, urlparse

from patchright.sync_api import sync_playwright
from patchright.sync_api import TimeoutError as PWTimeout

PROBE_URL = os.getenv("OZON_PROBE_URL", "").strip()
PROXY_URL = os.getenv("OZON_MINER_PROXY_URL", os.getenv("OZON_PROXY_URL", "")).strip()
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
BLOCK_RESOURCES = os.getenv("MINER_BLOCK_RESOURCES", "true").lower() in ("1", "true", "yes")
LOCALE = os.getenv("MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("MINER_TIMEZONE", "Europe/Moscow")
TIMEOUT_S = float(os.getenv("PROBE_TIMEOUT_SECONDS", "90"))
MAX_RELOADS = int(os.getenv("PROBE_MAX_RELOADS", "3"))
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

# Эндпоинты, на которых карточка отдаёт widgetStates (web: entrypoint-api,
# на части сборок ещё composer-api). Ловим именно их 200 как «FAB пройден».
API_MARKERS = ("entrypoint-api.bx/page/json", "composer-api.bx/page/json")
# Куки, на которых держится доверие FAB к веб-сессии (best-effort список —
# проба и покажет реальный набор, который потом реплеит Go-скрейпер).
TRUST_COOKIES = ("abt_data", "__Secure-ext_xcid", "__Secure-ab-group",
                 "__Secure-access-token", "xcid", "ADDRESSBOOKBAR_WEB_CLARIFICATION")
# Маркеры «стены» FAB/антибота в теле/заголовке (best-effort).
WALL_MARKERS = ("доступ ограничен", "подтвердите, что вы не робот",
                "что-то пошло не так", "access denied", "fab_")

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-probe")


def parse_proxy(url: str):
    if not url:
        return None
    u = urlparse(url)
    server = f"{u.scheme}://{u.hostname}"
    if u.port:
        server += f":{u.port}"
    proxy = {"server": server}
    if u.username:
        proxy["username"] = unquote(u.username)
    if u.password:
        proxy["password"] = unquote(u.password)
    return proxy


def _should_block(rt: str) -> bool:
    return rt in ("image", "media", "font")


_ID_RE = re.compile(r"/product/(?:[^/?#]*-)?(\d+)")


def product_id(url: str) -> str:
    m = _ID_RE.search(url)
    return m.group(1) if m else ""


# Метод друга: НЕ экспортируем куку в сторонний клиент, а делаем API-запрос
# ИЗНУТРИ уже доверенного браузера (fetch в контексте страницы). JA3+куки+решённый
# челлендж остаются согласованными. Проверяем, что из одного контекста можно
# скрейпить произвольный товар дешёвым XHR без перезагрузки страницы.
_INPAGE_FETCH_JS = """
async (id) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' +
              encodeURIComponent('/product/' + id + '/');
  try {
    const r = await fetch(url, {headers: {accept: 'application/json'},
                                credentials: 'include'});
    const body = await r.text();
    return {status: r.status,
            widgets: body.includes('widgetStates'),
            ruble: body.includes('\\u20bd'),
            fab: body.includes('fab_') || body.includes('incidentId'),
            len: body.length,
            snippet: body.slice(0, 200)};
  } catch (e) { return {status: -1, error: String(e)}; }
}
"""


def _human_nudge(page):
    try:
        page.mouse.move(random.randint(80, 1280), random.randint(80, 700),
                        steps=random.randint(4, 9))
        page.mouse.wheel(0, random.randint(200, 1100))
    except Exception:
        pass


def probe(pw) -> int:
    proxy = parse_proxy(PROXY_URL)
    launch = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
    if proxy:
        launch["proxy"] = proxy
    log.info("прокси: %s | headless=%s | url=%s",
             proxy["server"] if proxy else "НЕТ (напрямую)", HEADLESS, PROBE_URL)

    browser = pw.chromium.launch(**launch)
    try:
        context = browser.new_context(locale=LOCALE, timezone_id=TIMEZONE,
                                      viewport={"width": 1366, "height": 768})
        page = context.new_page()
        if BLOCK_RESOURCES:
            page.route("**/*", lambda r: (
                r.abort() if _should_block(r.request.resource_type) else r.continue_()))

        st = {"api_ok": False, "api_statuses": [], "fab": False,
              "responses": [], "price_in_api": False}

        def on_response(resp):
            try:
                st["responses"].append((resp.request.resource_type, resp.status, resp.url))
                if any(m in resp.url for m in API_MARKERS):
                    st["api_statuses"].append(resp.status)
                    if resp.status == 200:
                        st["api_ok"] = True
                        # цена видна анонимно? ищем ₽ в теле ответа карточки.
                        try:
                            body = resp.text()
                            if "fab_" in body or "incidentId" in body:
                                st["fab"] = True
                            if "₽" in body:
                                st["price_in_api"] = True
                        except Exception:
                            pass
                    elif resp.status == 403:
                        st["fab"] = True
            except Exception:
                pass

        page.on("response", on_response)

        per = max(30.0, TIMEOUT_S / max(1, MAX_RELOADS))
        for attempt in range(1, MAX_RELOADS + 1):
            try:
                page.goto(PROBE_URL, wait_until="domcontentloaded", timeout=int(per * 1000))
            except PWTimeout:
                pass
            except Exception as e:  # noqa: BLE001
                log.warning("goto ошибка (попытка %d): %s", attempt, e)
            end = time.time() + per
            while time.time() < end and not st["api_ok"]:
                _human_nudge(page)
                page.wait_for_timeout(2500)
            if st["api_ok"]:
                break
            log.info("попытка %d: 200 на API нет (статусы: %s)",
                     attempt, st["api_statuses"][-6:] or "—")

        # ── Метод друга: API-запрос из доверенного контекста (in-page fetch) ──
        inpage = {}
        pid = product_id(PROBE_URL)
        if pid:
            try:
                inpage = page.evaluate(_INPAGE_FETCH_JS, pid) or {}
            except Exception as e:  # noqa: BLE001
                inpage = {"status": -1, "error": str(e)}

        # ── Сбор фактов ──────────────────────────────────────────────────────
        cookies = [c for c in context.cookies() if "ozon.ru" in (c.get("domain") or "")]
        names = sorted(c["name"] for c in cookies)
        present_trust = [n for n in TRUST_COOKIES if n in names]
        ua = page.evaluate("() => navigator.userAgent")
        try:
            page_text = (page.evaluate("() => document.body ? document.body.innerText : ''") or "")
        except Exception:
            page_text = ""
        wall = any(m in (page_text.lower()) for m in WALL_MARKERS)
        price_on_page = "₽" in page_text

        hosts = Counter(urlparse(u).netloc for (_, _, u) in st["responses"])

        # ── Вердикт ──────────────────────────────────────────────────────────
        print("\n" + "=" * 64)
        print("OZON PROBE — РЕЗУЛЬТАТ")
        print("=" * 64)
        print(f"  exit-IP прокси     : {_egress_ip(context)}")
        print(f"  API 200 (FAB ok)   : {'ДА' if st['api_ok'] else 'НЕТ'}  "
              f"(статусы API: {st['api_statuses'] or '—'})")
        print(f"  FAB-челлендж        : {'ЕСТЬ ⚠️' if st['fab'] else 'нет'}")
        print(f"  стена/антибот текст : {'ЕСТЬ ⚠️' if wall else 'нет'}")
        print(f"  цена ₽ в API        : {'ДА' if st['price_in_api'] else 'нет'}")
        print(f"  цена ₽ на странице  : {'ДА' if price_on_page else 'нет'}")
        print(f"  in-page fetch (друг): status={inpage.get('status', '—')} "
              f"widgets={inpage.get('widgets')} ₽={inpage.get('ruble')} "
              f"fab={inpage.get('fab')} len={inpage.get('len', '—')}")
        if inpage.get("status") not in (200, None) or inpage.get("error"):
            print(f"    in-page snippet/err: {inpage.get('error') or inpage.get('snippet', '')!r}")
        print(f"  UA                  : {ua}")
        print(f"  cookie всего        : {len(names)}")
        print(f"  trust-cookie        : {present_trust or '— НИ ОДНОЙ ⚠️'}")
        print(f"  все cookie          : {names}")
        print(f"  ответы по хостам    : {dict(hosts.most_common(8))}")
        print("=" * 64)

        # in-page fetch (метод друга) — главный сигнал: можно ли скрейпить
        # произвольный товар из одного доверенного контекста.
        inpage_ok = inpage.get("status") == 200 and inpage.get("widgets") \
            and inpage.get("ruble") and not inpage.get("fab")
        nav_ok = st["api_ok"] and not st["fab"] and (st["price_in_api"] or price_on_page)

        if inpage_ok:
            print("ВЕРДИКТ: ✅✅ метод друга РАБОТАЕТ — из одного доверенного браузера "
                  "in-page fetch отдаёт widgetStates+цену без логина. Архитектура: "
                  "браузер-как-скрейпер (Python держит контекст, Go — тонкий клиент).")
            return 0
        if nav_ok:
            print("ВЕРДИКТ: ✅ FAB пройден и цена видна при навигации, но in-page fetch "
                  f"не отдал (status={inpage.get('status')}). Скрейпить навигацией на "
                  "/product/<id> (дороже) либо доискать правильные заголовки fetch.")
            return 0
        if st["fab"] or wall or inpage.get("fab"):
            print("ВЕРДИКТ: ❌ FAB душит даже реальный браузер через этот прокси → "
                  "egress спалён/датацентр. Нужен другой RU-резидентный/мобильный прокси.")
            return 2
        print("ВЕРДИКТ: ⚠️ FAB прошли, но цены нет анонимно → вероятно нужен логин "
              "(региональная/персональная цена). Майнеру понадобится залогиненный профиль.")
        return 3
    finally:
        try:
            browser.close()
        except Exception:
            pass


def _egress_ip(context) -> str:
    try:
        p = context.new_page()
        p.goto("https://api.ipify.org?format=json", timeout=15000)
        import json
        ip = json.loads(p.evaluate("() => document.body.innerText")).get("ip", "?")
        p.close()
        return ip
    except Exception:
        return "?"


def main():
    if not PROBE_URL:
        log.error("задай OZON_PROBE_URL=https://www.ozon.ru/product/...")
        sys.exit(1)
    with sync_playwright() as pw:
        sys.exit(probe(pw))


if __name__ == "__main__":
    main()
