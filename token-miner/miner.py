#!/usr/bin/env python3
"""
miner.py — авто-майнер WB-токенов поиска (этап 2: ПУЛ из N токенов).

Поддерживает пул из POOL_SIZE слотов в Redis:
  wb:search:pool:{i}  HASH{cookie, ua, token, status(ok|broken), mined_at, exp}
Цикл-«ремонтник» (каждые MINE_CHECK_INTERVAL_MINUTES): сканирует слоты и
перемайнивает только те, что требуют (пусто / status=broken / скоро экспирация /
слишком старый). Здоровый полный пул → почти ничего не делает.

Каждый майн идёт через headful Chromium в Xvfb + резидентный прокси (lteboost
ротирует exit-IP на сессию, поэтому слоты получают разные IP). Решение PoW-
челленджа wbaas происходит в браузере; успех = пойман 200 на /u-search/.

Дополнительно пишет для метрик/алертов и обратной совместимости:
  wb:search:pool:healthy          — сколько живых токенов в пуле
  wb:search:pool:oldest_mined_at  — самый старый mined_at среди живых
  wb:search:cookie/ua/token/...   — зеркало самого свежего живого токена (legacy)
"""

import base64
import logging
import os
import random
import re
import signal
import sys
import time
from collections import Counter
from urllib.parse import quote, unquote, urlparse

import redis
from patchright.sync_api import sync_playwright
from patchright.sync_api import TimeoutError as PWTimeout

# ── Конфиг из окружения ──────────────────────────────────────────────────────
REDIS_URL = os.getenv("REDIS_URL", "redis://redis:6379")
WB_SEARCH_QUERY = os.getenv("WB_SEARCH_QUERY", "телефон")
SEARCH_PAGE_URL = os.getenv(
    "WB_SEARCH_PAGE_URL",
    "https://www.wildberries.ru/catalog/0/search.aspx?search={query}",
)
USEARCH_MARKER = os.getenv("WB_USEARCH_MARKER", "/u-search/")

POOL_SIZE = int(os.getenv("WB_TOKEN_POOL_SIZE", "5"))
CHECK_INTERVAL_MIN = float(os.getenv("MINE_CHECK_INTERVAL_MINUTES", "5"))
REFRESH_MARGIN_H = float(os.getenv("MINE_REFRESH_MARGIN_HOURS", "24"))
MAX_AGE_H = float(os.getenv("MINE_MAX_AGE_HOURS", "48"))

MINE_RETRY_MINUTES = float(os.getenv("MINE_RETRY_MINUTES", "10"))
MINE_TIMEOUT_SECONDS = float(os.getenv("MINE_TIMEOUT_SECONDS", "150"))
MINE_MAX_RELOADS = int(os.getenv("MINE_MAX_RELOADS", "2"))
MINE_ONCE = os.getenv("MINE_ONCE", "false").lower() in ("1", "true", "yes")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
BLOCK_RESOURCES = os.getenv("MINER_BLOCK_RESOURCES", "true").lower() in ("1", "true", "yes")
DEBUG_DUMP = os.getenv("MINER_DEBUG_DUMP", "false").lower() in ("1", "true", "yes")
PROXY_URL = os.getenv("WB_TOKEN_PROXY_URL", "").strip()
BROWSER_CHANNEL = os.getenv("MINER_BROWSER_CHANNEL", "").strip()
WB_USER_AGENT = os.getenv("WB_USER_AGENT", "").strip()
LOCALE = os.getenv("MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("MINER_TIMEZONE", "Europe/Moscow")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()
DEBUG_DIR = os.getenv("MINER_DEBUG_DIR", "/app/debug")

# ── Ключи Redis ──────────────────────────────────────────────────────────────
SLOT_PREFIX = "wb:search:pool:"          # + i → HASH
KEY_HEALTHY = "wb:search:pool:healthy"
KEY_OLDEST = "wb:search:pool:oldest_mined_at"
# legacy-зеркало (читает ещё не обновлённый скрейпер + старые алерты):
KEY_COOKIE = "wb:search:cookie"
KEY_UA = "wb:search:ua"
KEY_TOKEN = "wb:search:token"
KEY_MINED_AT = "wb:search:token:mined_at"
KEY_EXP = "wb:search:token:exp"
TOKEN_COOKIE_NAME = "x_wbaas_token"

WALL_MARKERS = ("почти готов", "подозрительная активность", "что-то не так")

logging.basicConfig(
    level=getattr(logging, LOG_LEVEL, logging.INFO),
    format="%(asctime)s %(levelname)s %(message)s",
)
log = logging.getLogger("wb-token-miner")

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


def token_exp(token: str):
    try:
        parts = token.split(".")
        if len(parts) < 4:
            return None
        raw = parts[3]
        raw += "=" * (-len(raw) % 4)
        for decoder in (base64.urlsafe_b64decode, base64.b64decode):
            try:
                dec = decoder(raw).decode("utf-8", "ignore")
            except Exception:
                continue
            ts = [int(x) for x in re.findall(r"\b(17\d{8})\b", dec)]
            if ts:
                return max(ts)
        return None
    except Exception:
        return None


def _should_block(resource_type: str) -> bool:
    return resource_type in ("image", "media")


def _human_nudge(page):
    try:
        page.mouse.move(random.randint(80, 1280), random.randint(80, 700), steps=random.randint(4, 9))
    except Exception:
        pass
    try:
        page.mouse.wheel(0, random.randint(200, 1100))
    except Exception:
        pass


def _on_wall(page) -> bool:
    try:
        t = (page.title() or "").lower()
        if any(m in t for m in WALL_MARKERS):
            return True
        body = page.evaluate("() => document.body ? document.body.innerText : ''") or ""
        return any(m in body.lower() for m in WALL_MARKERS)
    except Exception:
        return False


def _dump_diagnostics(page, context, state, tag: str):
    try:
        hosts = Counter()
        for (rtype, status, url) in state["responses"]:
            try:
                host = urlparse(url).netloc
            except Exception:
                host = url[:40]
            hosts[host] += 1
        log.warning("─── ДИАГНОСТИКА (%s) ───", tag)
        log.warning("ответов всего: %d, по хостам: %s", len(state["responses"]), dict(hosts.most_common(8)))
        for rt, st, u in [(rt, st, urlparse(u).netloc + urlparse(u).path)
                          for (rt, st, u) in state["responses"]
                          if rt in ("document", "xhr", "fetch", "script", "other")][:25]:
            log.warning("  %-8s %s  %s", rt, st, u[:90])
        try:
            log.warning("page.url=%s title=%r", page.url, page.title())
            body = page.evaluate("() => document.body ? document.body.innerText : ''") or ""
            log.warning("body[:300]=%r", body[:300].replace("\n", " "))
        except Exception as e:  # noqa: BLE001
            log.warning("тело страницы недоступно: %s", e)
        try:
            os.makedirs(DEBUG_DIR, exist_ok=True)
            ts = time.strftime("%Y%m%d-%H%M%S")
            page.screenshot(path=os.path.join(DEBUG_DIR, f"{ts}-{tag}.png"), full_page=False)
            with open(os.path.join(DEBUG_DIR, f"{ts}-{tag}.html"), "w", encoding="utf-8") as f:
                f.write(page.content())
        except Exception as e:  # noqa: BLE001
            log.warning("скриншот/html не сохранён: %s", e)
        log.warning("─── /ДИАГНОСТИКА ───")
    except Exception as e:  # noqa: BLE001
        log.warning("диагностика упала: %s", e)


def mine_once(pw) -> dict | None:
    """Одна добыча токена (свежий браузер → свежий exit-IP прокси)."""
    proxy = parse_proxy(PROXY_URL)
    launch_kwargs = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
    if proxy:
        launch_kwargs["proxy"] = proxy
    if BROWSER_CHANNEL:
        launch_kwargs["channel"] = BROWSER_CHANNEL

    browser = pw.chromium.launch(**launch_kwargs)
    try:
        ctx_kwargs = {"locale": LOCALE, "timezone_id": TIMEZONE, "viewport": {"width": 1366, "height": 768}}
        if WB_USER_AGENT:
            ctx_kwargs["user_agent"] = WB_USER_AGENT
        context = browser.new_context(**ctx_kwargs)
        page = context.new_page()

        if BLOCK_RESOURCES:
            page.route("**/*", lambda route: (
                route.abort() if _should_block(route.request.resource_type) else route.continue_()
            ))

        state = {"ok": False, "statuses": [], "responses": [], "failed": []}

        def on_response(resp):
            try:
                state["responses"].append((resp.request.resource_type, resp.status, resp.url))
                if USEARCH_MARKER in resp.url:
                    state["statuses"].append(resp.status)
                    if resp.status == 200:
                        state["ok"] = True
            except Exception:
                pass

        page.on("response", on_response)
        page.on("requestfailed", lambda req: state["failed"].append(getattr(req, "url", "")))

        url = SEARCH_PAGE_URL.format(query=quote(WB_SEARCH_QUERY))
        per_attempt = max(45.0, MINE_TIMEOUT_SECONDS / max(1, MINE_MAX_RELOADS))

        for attempt in range(1, MINE_MAX_RELOADS + 1):
            try:
                page.goto(url, wait_until="domcontentloaded", timeout=int(per_attempt * 1000))
            except PWTimeout:
                pass
            except Exception as e:  # noqa: BLE001
                log.warning("goto ошибка (попытка %d): %s", attempt, e)
            end = time.time() + per_attempt
            while time.time() < end and not state["ok"] and not _stop["flag"]:
                _human_nudge(page)
                page.wait_for_timeout(2500)
            if state["ok"] or _stop["flag"]:
                break
            wall = _on_wall(page)
            log.info("попытка %d: 200 нет (u-search: %s)%s", attempt, state["statuses"][-6:] or "—",
                     " — стена wbaas" if wall else "")
            page.wait_for_timeout(2000)

        if not state["ok"]:
            if DEBUG_DUMP:
                _dump_diagnostics(page, context, state, "fail")
            return None

        wb = [c for c in context.cookies() if "wildberries.ru" in (c.get("domain") or "")]
        token = next((c["value"] for c in wb if c["name"] == TOKEN_COOKIE_NAME), "")
        if not token:
            if DEBUG_DUMP:
                _dump_diagnostics(page, context, state, "no-token")
            return None
        cookie_header = "; ".join(f'{c["name"]}={c["value"]}' for c in wb)
        ua = page.evaluate("() => navigator.userAgent")
        return {"cookie": cookie_header, "ua": ua, "token": token}
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
    if not h.get("cookie"):
        return True, "нет cookie"
    exp = _int(h.get("exp"))
    if exp and exp - now < REFRESH_MARGIN_H * 3600:
        return True, "скоро экспирация"
    mined = _int(h.get("mined_at"))
    if mined and now - mined > MAX_AGE_H * 3600:
        return True, "старый"
    return False, ""


def slot_alive(h: dict, now: int) -> bool:
    if not h or h.get("status") != "ok" or not h.get("cookie"):
        return False
    exp = _int(h.get("exp"))
    return not (exp and exp < now)


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
                exp = token_exp(data["token"]) or 0
                try:
                    r.hset(slot_key(i), mapping={
                        "cookie": data["cookie"], "ua": data["ua"], "token": data["token"],
                        "status": "ok", "mined_at": now, "exp": exp,
                    })
                    h = {"cookie": data["cookie"], "ua": data["ua"], "token": data["token"],
                         "status": "ok", "mined_at": str(now), "exp": str(exp)}
                    log.info("слот %d обновлён: %s…(%d симв.) exp≈%s", i, data["token"][:20],
                             len(data["token"]),
                             time.strftime("%Y-%m-%d %H:%M", time.localtime(exp)) if exp else "?")
                except Exception as e:  # noqa: BLE001
                    log.error("слот %d: запись в Redis упала: %s", i, e)
            else:
                log.warning("слот %d: майнинг не удался — оставляю как есть", i)
        slots.append((i, h))

    alive = [(i, h) for (i, h) in slots if slot_alive(h, now)]
    healthy = len(alive)
    try:
        r.set(KEY_HEALTHY, healthy)
        mined_vals = [_int(h.get("mined_at")) for (_, h) in alive if _int(h.get("mined_at"))]
        if mined_vals:
            r.set(KEY_OLDEST, min(mined_vals))
        if alive:
            # зеркало самого свежего живого токена в legacy-ключи
            fi, fh = max(alive, key=lambda x: _int(x[1].get("mined_at")))
            r.set(KEY_COOKIE, fh["cookie"])
            r.set(KEY_UA, fh.get("ua", ""))
            r.set(KEY_TOKEN, fh.get("token", ""))
            r.set(KEY_MINED_AT, _int(fh.get("mined_at"), now))
            if _int(fh.get("exp")):
                r.set(KEY_EXP, _int(fh.get("exp")))
    except Exception as e:  # noqa: BLE001
        log.error("запись метрик/зеркала упала: %s", e)

    log.info("цикл готов: живых токенов %d/%d", healthy, POOL_SIZE)
    return healthy


def interruptible_sleep(seconds: float):
    end = time.time() + seconds
    while time.time() < end and not _stop["flag"]:
        time.sleep(min(1.0, end - time.time()))


def main():
    signal.signal(signal.SIGTERM, _handle_signal)
    signal.signal(signal.SIGINT, _handle_signal)

    r = redis.from_url(REDIS_URL, decode_responses=True, socket_connect_timeout=5)
    try:
        r.ping()
        log.info("Redis на связи: %s", REDIS_URL)
    except Exception as e:  # noqa: BLE001
        log.error("Redis недоступен (%s): %s", REDIS_URL, e)
        if MINE_ONCE:
            sys.exit(1)

    log.info(
        "старт майнера-пула: pool=%d query=%r check=%.0fмин refresh<%.0fч maxage=%.0fч "
        "reloads=%d timeout=%.0fс channel=%s headless=%s display=%s once=%s proxy=%s",
        POOL_SIZE, WB_SEARCH_QUERY, CHECK_INTERVAL_MIN, REFRESH_MARGIN_H, MAX_AGE_H,
        MINE_MAX_RELOADS, MINE_TIMEOUT_SECONDS, BROWSER_CHANNEL or "chromium",
        HEADLESS, os.getenv("DISPLAY", "—"), MINE_ONCE, bool(PROXY_URL),
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
            # если пул совсем пуст — повторяем быстрее (retry), иначе обычный интервал
            sleep_min = MINE_RETRY_MINUTES if healthy == 0 else CHECK_INTERVAL_MIN
            interruptible_sleep(sleep_min * 60)

    log.info("майнер остановлен")


if __name__ == "__main__":
    main()
