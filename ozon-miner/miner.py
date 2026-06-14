#!/usr/bin/env python3
"""
miner.py — авто-майнер антибот-токена Ozon (cookie __Secure-ETC).

Архитектура (= паттерн WB token-miner, см. ../token-miner/miner.py):
FAB (антибот Ozon) на первом контакте отдаёт fab_chlg_ — обфусцированный JS-VM
challenge.html, который нельзя пройти на чистом Go. Но реальный браузер решает его
прозрачно при обычной навигации и получает cookie __Secure-ETC. Майнер изредка
гоняет headful Chromium (Patchright) в Xvfb через мобильный РФ-прокси, харвестит
__Secure-ETC и кладёт в Redis. Go-скрейпер (bogdanfinn/tls-client, Chrome-профиль,
ТОТ ЖЕ прокси) переиспользует cookie для частых дешёвых запросов товаров.

ETC привязан к IP, поэтому майнер и скрейпер обязаны ходить через ОДИН выход
(приватный мобильный прокси держит IP стабильным). Пул для Ozon = 1 слот: все
токены были бы с одного sticky-IP, пул из N ничего не разносит (Ozon лимитит по IP).
Размер настраивается OZON_POOL_SIZE на случай нескольких прокси.

Слот Redis: {prefix}pool:{i}  HASH{cookie, ua, etc, ip, mined_at, status(ok|broken)}
Метрики:    {prefix}pool:healthy, {prefix}pool:mined_total, {prefix}pool:mine_failed_total
"""

import logging
import os
import random
import signal
import sys
import time
from collections import Counter
from urllib.parse import unquote, urlparse

import redis
from patchright.sync_api import sync_playwright
from patchright.sync_api import TimeoutError as PWTimeout

# ── Конфиг из окружения ──────────────────────────────────────────────────────
REDIS_URL = os.getenv("REDIS_URL", "redis://redis:6379")
# Страница для «прогрева» FAB. Главная — самая лёгкая и точно ставит __Secure-ETC
# на весь домен .ozon.ru. Можно подменить на карточку товара для проверки PDP.
WARMUP_URL = os.getenv("OZON_WARMUP_URL", "https://www.ozon.ru/")
# Карточка для пост-валидации сессии через entrypoint-api (best-effort, не валит майн).
VALIDATE_PRODUCT = os.getenv("OZON_VALIDATE_PRODUCT", "").strip()

POOL_SIZE = int(os.getenv("OZON_POOL_SIZE", "1"))
CHECK_INTERVAL_MIN = float(os.getenv("MINE_CHECK_INTERVAL_MINUTES", "5"))
MAX_AGE_H = float(os.getenv("MINE_MAX_AGE_HOURS", "6"))

MINE_RETRY_MINUTES = float(os.getenv("MINE_RETRY_MINUTES", "5"))
MINE_TIMEOUT_SECONDS = float(os.getenv("MINE_TIMEOUT_SECONDS", "150"))
MINE_MAX_RELOADS = int(os.getenv("MINE_MAX_RELOADS", "3"))
MINE_ONCE = os.getenv("MINE_ONCE", "false").lower() in ("1", "true", "yes")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
DEBUG_DUMP = os.getenv("MINER_DEBUG_DUMP", "false").lower() in ("1", "true", "yes")
PROXY_URL = os.getenv("OZON_PROXY_URL", "").strip()
BROWSER_CHANNEL = os.getenv("MINER_BROWSER_CHANNEL", "").strip()
LOCALE = os.getenv("MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("MINER_TIMEZONE", "Europe/Moscow")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()
DEBUG_DIR = os.getenv("MINER_DEBUG_DIR", "/app/debug")

# ── Ключи Redis ──────────────────────────────────────────────────────────────
# Префикс должен совпадать с тем, что читает Go OzonScraper (internal/scraper/ozon.go).
POOL_PREFIX = os.getenv("OZON_POOL_PREFIX", "ozon:etc:")
SLOT_PREFIX = f"{POOL_PREFIX}pool:"
KEY_HEALTHY = f"{POOL_PREFIX}pool:healthy"
KEY_MINED_TOTAL = f"{POOL_PREFIX}pool:mined_total"
KEY_MINE_FAILED = f"{POOL_PREFIX}pool:mine_failed_total"

ETC_COOKIE_NAME = "__Secure-ETC"
# Маркеры страницы блокировки/челленджа FAB (если браузер не прошёл).
BLOCK_MARKERS = ("доступ ограничен", "access denied", "incidentid", "fab_nmk", "fab_chlg")
# Сетевой маркер успеха: реальный 200 от storefront-API (как /u-search/ у WB).
API_MARKER = "entrypoint-api.bx"

logging.basicConfig(
    level=getattr(logging, LOG_LEVEL, logging.INFO),
    format="%(asctime)s %(levelname)s %(message)s",
)
log = logging.getLogger("ozon-etc-miner")

_stop = {"flag": False}


def _handle_signal(signum, _frame):
    log.info("получен сигнал %s — завершаюсь после текущего шага", signum)
    _stop["flag"] = True


def _int(v, default=0):
    try:
        return int(str(v).strip())
    except Exception:
        return default


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


def _human_nudge(page):
    try:
        page.mouse.move(random.randint(80, 1280), random.randint(80, 700), steps=random.randint(4, 9))
    except Exception:
        pass
    try:
        page.mouse.wheel(0, random.randint(200, 1100))
    except Exception:
        pass


def _on_block(page) -> bool:
    try:
        t = (page.title() or "").lower()
        if any(m in t for m in BLOCK_MARKERS):
            return True
        body = page.evaluate("() => document.body ? document.body.innerText : ''") or ""
        return any(m in body.lower() for m in BLOCK_MARKERS)
    except Exception:
        return False


def _egress_ip(context) -> str:
    """IP выхода прокси — кладём в слот, чтобы скрейпер знал, к какому IP привязан ETC.
    Несколько источников с ретраями: через мобильный прокси одиночный сервис
    нередко таймаутит, а пустой IP ломает диагностику ротации на стороне Go."""
    for url, key in (
        ("https://api.ipify.org?format=json", "ip"),
        ("https://ipinfo.io/json", "ip"),
        ("https://api.myip.com", "ip"),
    ):
        for _ in range(2):
            try:
                resp = context.request.get(url, timeout=15000)
                if resp.ok:
                    ip = (resp.json() or {}).get(key, "")
                    if ip:
                        return ip
            except Exception:
                pass
    return ""


def _dump_diagnostics(page, state, tag: str):
    try:
        hosts = Counter()
        for (_rt, _st, url) in state["responses"]:
            try:
                hosts[urlparse(url).netloc] += 1
            except Exception:
                hosts[url[:40]] += 1
        log.warning("─── ДИАГНОСТИКА (%s) ───", tag)
        log.warning("ответов: %d, по хостам: %s", len(state["responses"]), dict(hosts.most_common(8)))
        try:
            log.warning("page.url=%s title=%r", page.url, page.title())
            body = page.evaluate("() => document.body ? document.body.innerText : ''") or ""
            log.warning("body[:300]=%r", body[:300].replace("\n", " "))
        except Exception as e:  # noqa: BLE001
            log.warning("тело недоступно: %s", e)
        try:
            os.makedirs(DEBUG_DIR, exist_ok=True)
            ts = time.strftime("%Y%m%d-%H%M%S")
            page.screenshot(path=os.path.join(DEBUG_DIR, f"{ts}-{tag}.png"), full_page=False)
            with open(os.path.join(DEBUG_DIR, f"{ts}-{tag}.html"), "w", encoding="utf-8") as f:
                f.write(page.content())
        except Exception as e:  # noqa: BLE001
            log.warning("скрин/html не сохранён: %s", e)
        log.warning("─── /ДИАГНОСТИКА ───")
    except Exception as e:  # noqa: BLE001
        log.warning("диагностика упала: %s", e)


def _validate_session(page, slot_label: str):
    """Проверка пути A: дёргаем entrypoint-api **изнутри живой страницы** (fetch в
    JS-контексте уже прошедшей FAB вкладки ozon.ru) — это НЕ «голый» запрос, а
    same-origin как у настоящего сайта, поэтому FAB его должен пропустить.
    Логируем status + есть ли widgetStates. Не валит майн."""
    if not VALIDATE_PRODUCT:
        return
    js = """async (id) => {
      try {
        const r = await fetch(`/api/entrypoint-api.bx/page/json/v2?url=/product/${id}/`,
                              {headers: {accept: 'application/json'}, credentials: 'include'});
        const t = await r.text();
        return {status: r.status, has: t.includes('widgetStates'), len: t.length};
      } catch (e) { return {status: -1, has: false, len: 0, err: String(e)}; }
    }"""
    try:
        res = page.evaluate(js, VALIDATE_PRODUCT)
        log.info("валидация %s (in-page fetch): status=%s widgetStates=%s len=%s",
                 slot_label, res.get("status"), res.get("has"), res.get("len"))
    except Exception as e:  # noqa: BLE001
        log.info("валидация %s: page.evaluate упал: %s", slot_label, e)


def mine_once(pw) -> dict | None:
    """Одна добыча ETC: свежий браузер через мобильный прокси решает FAB и
    отдаёт cookie-строку домена .ozon.ru + UA + значение __Secure-ETC + egress-IP."""
    proxy = parse_proxy(PROXY_URL)
    launch_kwargs = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
    if proxy:
        launch_kwargs["proxy"] = proxy
    if BROWSER_CHANNEL:
        launch_kwargs["channel"] = BROWSER_CHANNEL

    browser = pw.chromium.launch(**launch_kwargs)
    try:
        context = browser.new_context(
            locale=LOCALE, timezone_id=TIMEZONE, viewport={"width": 1366, "height": 768}
        )
        page = context.new_page()

        state = {"api_ok": False, "responses": []}

        def on_response(resp):
            try:
                state["responses"].append((resp.request.resource_type, resp.status, resp.url))
                if API_MARKER in resp.url and resp.status == 200:
                    state["api_ok"] = True
            except Exception:
                pass

        page.on("response", on_response)

        per_attempt = max(40.0, MINE_TIMEOUT_SECONDS / max(1, MINE_MAX_RELOADS))
        etc = ""

        for attempt in range(1, MINE_MAX_RELOADS + 1):
            try:
                page.goto(WARMUP_URL, wait_until="domcontentloaded", timeout=int(per_attempt * 1000))
            except PWTimeout:
                pass
            except Exception as e:  # noqa: BLE001
                log.warning("goto ошибка (попытка %d): %s", attempt, e)

            end = time.time() + per_attempt
            while time.time() < end and not _stop["flag"]:
                _human_nudge(page)
                page.wait_for_timeout(2000)
                etc = next((c["value"] for c in context.cookies()
                            if c["name"] == ETC_COOKIE_NAME), "")
                # успех = есть ETC и (прошёл API ИЛИ страница не блок)
                if etc and (state["api_ok"] or not _on_block(page)):
                    break
            if etc and (state["api_ok"] or not _on_block(page)):
                break
            log.info("попытка %d: ETC=%s api_ok=%s block=%s",
                     attempt, bool(etc), state["api_ok"], _on_block(page))
            page.wait_for_timeout(1500)

        if not etc:
            if DEBUG_DUMP:
                _dump_diagnostics(page, state, "no-etc")
            return None

        ozon_cookies = [c for c in context.cookies() if "ozon.ru" in (c.get("domain") or "")]
        cookie_header = "; ".join(f'{c["name"]}={c["value"]}' for c in ozon_cookies)
        ua = page.evaluate("() => navigator.userAgent")
        ip = _egress_ip(context)
        _validate_session(page, "mine")
        return {"cookie": cookie_header, "ua": ua, "etc": etc, "ip": ip}
    finally:
        try:
            browser.close()
        except Exception:
            pass


def slot_key(i: int) -> str:
    return f"{SLOT_PREFIX}{i}"


def slot_needs_mine(h: dict, now: int):
    if not h:
        return True, "пусто"
    if h.get("status") != "ok":
        return True, f"status={h.get('status')}"
    if not h.get("cookie") or not h.get("etc"):
        return True, "нет cookie/etc"
    mined = _int(h.get("mined_at"))
    if mined and now - mined > MAX_AGE_H * 3600:
        return True, "старый"
    return False, ""


def slot_alive(h: dict) -> bool:
    return bool(h and h.get("status") == "ok" and h.get("cookie") and h.get("etc"))


def run_cycle(r: "redis.Redis", pw):
    now = int(time.time())
    slots = []
    for i in range(POOL_SIZE):
        if _stop["flag"]:
            break
        try:
            h = r.hgetall(slot_key(i))
        except Exception as e:  # noqa: BLE001
            log.error("слот %d: чтение Redis упало: %s", i, e)
            h = {}
        need, reason = slot_needs_mine(h, now)
        if need:
            log.info("слот %d: майню (%s)", i, reason)
            data = mine_once(pw)
            if data:
                try:
                    r.hset(slot_key(i), mapping={
                        "cookie": data["cookie"], "ua": data["ua"], "etc": data["etc"],
                        "ip": data["ip"], "status": "ok", "mined_at": now,
                    })
                    r.incr(KEY_MINED_TOTAL)
                    h = {"cookie": data["cookie"], "ua": data["ua"], "etc": data["etc"],
                         "ip": data["ip"], "status": "ok", "mined_at": str(now)}
                    log.info("слот %d обновлён: etc=%s…(%d) ip=%s", i, data["etc"][:12],
                             len(data["etc"]), data["ip"] or "?")
                except Exception as e:  # noqa: BLE001
                    log.error("слот %d: запись в Redis упала: %s", i, e)
            else:
                log.warning("слот %d: майнинг не удался — оставляю как есть", i)
                try:
                    r.incr(KEY_MINE_FAILED)
                except Exception as e:  # noqa: BLE001
                    log.error("счётчик фейлов: запись упала: %s", e)
        slots.append((i, h))

    healthy = sum(1 for (_, h) in slots if slot_alive(h))
    try:
        r.set(KEY_HEALTHY, healthy)
    except Exception as e:  # noqa: BLE001
        log.error("запись метрик упала: %s", e)

    log.info("цикл готов: живых ETC %d/%d", healthy, POOL_SIZE)
    return healthy


def interruptible_sleep(seconds: float):
    end = time.time() + seconds
    while time.time() < end and not _stop["flag"]:
        time.sleep(min(1.0, end - time.time()))


def main():
    signal.signal(signal.SIGTERM, _handle_signal)
    signal.signal(signal.SIGINT, _handle_signal)

    if not PROXY_URL:
        log.warning("OZON_PROXY_URL пуст — без мобильного РФ-прокси FAB даст fab_nmk_ (блок). "
                    "Майнер будет работать, но почти наверняка впустую.")

    r = redis.from_url(REDIS_URL, decode_responses=True, socket_connect_timeout=5)
    try:
        r.ping()
        log.info("Redis на связи: %s", REDIS_URL)
    except Exception as e:  # noqa: BLE001
        log.error("Redis недоступен (%s): %s", REDIS_URL, e)
        if MINE_ONCE:
            sys.exit(1)

    log.info(
        "старт ozon-miner: prefix=%s pool=%d warmup=%s check=%.0fмин maxage=%.0fч "
        "reloads=%d timeout=%.0fс channel=%s headless=%s once=%s proxy=%s",
        POOL_PREFIX, POOL_SIZE, WARMUP_URL, CHECK_INTERVAL_MIN, MAX_AGE_H,
        MINE_MAX_RELOADS, MINE_TIMEOUT_SECONDS, BROWSER_CHANNEL or "chromium",
        HEADLESS, MINE_ONCE, bool(PROXY_URL),
    )

    with sync_playwright() as pw:
        while not _stop["flag"]:
            try:
                healthy = run_cycle(r, pw)
            except Exception as e:  # noqa: BLE001
                log.exception("цикл упал: %s", e)
                healthy = 0
            if MINE_ONCE:
                break
            sleep_min = MINE_RETRY_MINUTES if healthy == 0 else CHECK_INTERVAL_MIN
            interruptible_sleep(sleep_min * 60)

    log.info("ozon-miner остановлен")


if __name__ == "__main__":
    main()
