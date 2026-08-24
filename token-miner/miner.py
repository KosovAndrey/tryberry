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
import urllib.error
import urllib.request
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
# Потолок паузы, когда минт не удаётся ЦИКЛ ЗА ЦИКЛОМ (стена wbaas). Без него
# пустой пул ускорял ретраи (healthy==0 → MINE_RETRY_MINUTES), и майнер долбил
# стену навигациями каждые 5 минут — ровно то, чего делать нельзя (инцидент
# 24-08: 5 слотов × 2 навигации каждые 5 мин сорок минут подряд).
MINE_WALL_BACKOFF_MAX_MINUTES = float(os.getenv("MINE_WALL_BACKOFF_MAX_MINUTES", "60"))
# Перед минтом слота, помеченного broken, проверяем его cookie ДЕШЁВЫМ запросом:
# 429 от воркера мог быть rate-limit'ом, а не протухшим токеном, и тогда слот
# чинится без браузера (инцидент 24-08 сжёг так весь перекупный пул).
MINE_REVALIDATE = os.getenv("MINE_REVALIDATE", "true").lower() in ("1", "true", "yes")
MINE_VALIDATE_URL = os.getenv(
    "MINE_VALIDATE_URL",
    "https://www.wildberries.ru/__internal/u-search/exactmatch/ru/common/v18/search"
    "?appType=1&curr=rub&dest=-1257786&lang=ru&locale=ru&page=1"
    "&query=%D0%BA%D0%B0%D0%BF%D0%B8%D0%B1%D0%B0%D1%80%D0%B0&resultset=catalog&sort=popular&spp=30",
)
MINE_VALIDATE_TIMEOUT_S = float(os.getenv("MINE_VALIDATE_TIMEOUT_SECONDS", "15"))
MINE_TIMEOUT_SECONDS = float(os.getenv("MINE_TIMEOUT_SECONDS", "150"))
MINE_MAX_RELOADS = int(os.getenv("MINE_MAX_RELOADS", "2"))
MINE_ONCE = os.getenv("MINE_ONCE", "false").lower() in ("1", "true", "yes")

# Общий лок майнинга (один ключ Redis на оба WB-майнера): не даём обычному и
# reseller-майнеру майнить одновременно с одного IP — всплеск токен-минтов палит WB.
# Пусто → лок выключен (старое поведение). TTL — страховка, если процесс умрёт с локом.
MINE_GLOBAL_LOCK = os.getenv("MINE_GLOBAL_LOCK", "").strip()
MINE_LOCK_TTL_MIN = float(os.getenv("MINE_LOCK_TTL_MINUTES", "30"))

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
# Префикс параметризован: обычный пул — "wb:search:", пул перекупов —
# "wb:reseller:". Должен совпадать с WB_TOKEN_POOL_PREFIX у Go-воркера.
POOL_PREFIX = os.getenv("WB_TOKEN_POOL_PREFIX", "wb:search:")
# Для пула перекупов legacy-зеркало не нужно (его читают только старый скрейпер
# и алерты обычной дорожки) — отключается флагом.
DISABLE_LEGACY = os.getenv("WB_DISABLE_LEGACY_MIRROR", "false").lower() in ("1", "true", "yes")

SLOT_PREFIX = f"{POOL_PREFIX}pool:"          # + i → HASH
KEY_HEALTHY = f"{POOL_PREFIX}pool:healthy"
KEY_OLDEST = f"{POOL_PREFIX}pool:oldest_mined_at"
# Счётчики майнов (только вверх) — redis-exporter отдаёт их в Prometheus,
# increase() по ним = сколько раз сходили через прокси (расход трафика).
KEY_MINED_TOTAL = f"{POOL_PREFIX}pool:mined_total"
KEY_MINE_FAILED = f"{POOL_PREFIX}pool:mine_failed_total"
# legacy-зеркало (читает ещё не обновлённый скрейпер + старые алерты):
KEY_COOKIE = f"{POOL_PREFIX}cookie"
KEY_UA = f"{POOL_PREFIX}ua"
KEY_TOKEN = f"{POOL_PREFIX}token"
KEY_MINED_AT = f"{POOL_PREFIX}token:mined_at"
KEY_EXP = f"{POOL_PREFIX}token:exp"
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


def _acquire_mine_lock(r) -> bool:
    """Берём общий лок майнинга (SET NX EX). True — взяли (или лок выключен/Redis-сбой:
    не блокируем майнинг). False — лок держит другой майнер → цикл пропускаем."""
    if not MINE_GLOBAL_LOCK:
        return True
    try:
        return bool(r.set(MINE_GLOBAL_LOCK, POOL_PREFIX or "default",
                          nx=True, ex=int(MINE_LOCK_TTL_MIN * 60)))
    except Exception as e:  # noqa: BLE001
        log.error("лок майнинга: ошибка получения, майню без лока: %s", e)
        return True


def _release_mine_lock(r):
    if not MINE_GLOBAL_LOCK:
        return
    try:
        r.delete(MINE_GLOBAL_LOCK)
    except Exception as e:  # noqa: BLE001
        log.error("лок майнинга: ошибка снятия: %s", e)


# Итоги минта последнего цикла — по ним main решает, стоит ли стена.
_cycle = {"mint_ok": 0, "mint_fail": 0}


def revalidate_cookie(h: dict) -> bool:
    """Жива ли cookie слота: один дешёвый GET к u-search (без браузера).
    True — 200, слот можно вернуть в строй. False — всё остальное (в т.ч. стена
    и сетевые ошибки): решение о минте принимает вызывающий."""
    cookie = h.get("cookie") or ""
    if not cookie:
        return False
    req = urllib.request.Request(MINE_VALIDATE_URL, headers={
        "Accept": "*/*",
        "Accept-Language": "ru,en;q=0.9",
        "User-Agent": h.get("ua") or "Mozilla/5.0",
        "Cookie": cookie,
        "X-Requested-With": "XMLHttpRequest",
    })
    opener = urllib.request.build_opener(
        urllib.request.ProxyHandler({"https": PROXY_URL, "http": PROXY_URL} if PROXY_URL else {})
    )
    try:
        with opener.open(req, timeout=MINE_VALIDATE_TIMEOUT_S) as resp:
            return resp.status == 200
    except urllib.error.HTTPError as e:
        log.info("ревалидация: %s", e.code)
        return False
    except Exception as e:  # noqa: BLE001
        log.info("ревалидация не удалась: %s", e)
        return False


def run_cycle(r: "redis.Redis", pw):
    now = int(time.time())
    _cycle["mint_ok"] = _cycle["mint_fail"] = 0
    # 1) читаем все слоты, решаем какие нуждаются в майнинге
    state_by_i: dict = {}
    needy: list = []
    for i in range(POOL_SIZE):
        if _stop["flag"]:
            break
        try:
            h = r.hgetall(slot_key(i))
        except Exception as e:  # noqa: BLE001
            log.error("слот %d: чтение Redis упало: %s", i, e)
            h = {}
        state_by_i[i] = h
        need, reason = slot_needs_mine(h, now)
        # Слот помечен broken, но cookie на месте и не протухла → сперва проверяем
        # её живым запросом: минт в браузере — самая дорогая и рискованная
        # операция, и гонять её из-за чужого 429 незачем.
        if need and MINE_REVALIDATE and reason == "status=broken" and h.get("cookie"):
            exp = _int(h.get("exp"))
            if not exp or exp > now:
                if revalidate_cookie(h):
                    try:
                        r.hset(slot_key(i), "status", "ok")
                        h["status"] = "ok"
                        state_by_i[i] = h
                        log.info("слот %d: cookie жива — вернул в строй без минта", i)
                        need = False
                    except Exception as e:  # noqa: BLE001
                        log.error("слот %d: возврат в строй не удался: %s", i, e)
        if need:
            needy.append((i, reason))

    # 2) общий лок: не майним одновременно со вторым WB-майнером (делим один IP)
    lock = False
    if needy:
        lock = _acquire_mine_lock(r)
        if not lock:
            log.info("нужно майнить %d слот(ов), но лок держит другой WB-майнер — "
                     "пропускаю цикл (домайню в следующем)", len(needy))
            needy = []

    # 3) майним нуждающиеся слоты под локом
    try:
        for i, reason in needy:
            if _stop["flag"]:
                break
            log.info("слот %d: майню (%s)", i, reason)
            data = mine_once(pw)
            if data:
                _cycle["mint_ok"] += 1
                exp = token_exp(data["token"]) or 0
                try:
                    r.hset(slot_key(i), mapping={
                        "cookie": data["cookie"], "ua": data["ua"], "token": data["token"],
                        "status": "ok", "mined_at": now, "exp": exp,
                    })
                    r.incr(KEY_MINED_TOTAL)
                    state_by_i[i] = {"cookie": data["cookie"], "ua": data["ua"], "token": data["token"],
                                     "status": "ok", "mined_at": str(now), "exp": str(exp)}
                    log.info("слот %d обновлён: %s…(%d симв.) exp≈%s", i, data["token"][:20],
                             len(data["token"]),
                             time.strftime("%Y-%m-%d %H:%M", time.localtime(exp)) if exp else "?")
                except Exception as e:  # noqa: BLE001
                    log.error("слот %d: запись в Redis упала: %s", i, e)
            else:
                _cycle["mint_fail"] += 1
                log.warning("слот %d: майнинг не удался — оставляю как есть", i)
                try:
                    r.incr(KEY_MINE_FAILED)
                except Exception as e:  # noqa: BLE001
                    log.error("счётчик фейлов: запись в Redis упала: %s", e)
    finally:
        if lock:
            _release_mine_lock(r)

    slots = sorted(state_by_i.items())
    alive = [(i, h) for (i, h) in slots if slot_alive(h, now)]
    healthy = len(alive)
    try:
        r.set(KEY_HEALTHY, healthy)
        mined_vals = [_int(h.get("mined_at")) for (_, h) in alive if _int(h.get("mined_at"))]
        if mined_vals:
            r.set(KEY_OLDEST, min(mined_vals))
        if alive and not DISABLE_LEGACY:
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
        "старт майнера-пула: prefix=%s pool=%d query=%r check=%.0fмин refresh<%.0fч maxage=%.0fч "
        "reloads=%d timeout=%.0fс channel=%s headless=%s display=%s once=%s proxy=%s legacy=%s",
        POOL_PREFIX, POOL_SIZE, WB_SEARCH_QUERY, CHECK_INTERVAL_MIN, REFRESH_MARGIN_H, MAX_AGE_H,
        MINE_MAX_RELOADS, MINE_TIMEOUT_SECONDS, BROWSER_CHANNEL or "chromium",
        HEADLESS, os.getenv("DISPLAY", "—"), MINE_ONCE, bool(PROXY_URL), not DISABLE_LEGACY,
    )

    wall_cycles = 0  # подряд идущих циклов, где не прошёл ни один минт
    with sync_playwright() as pw:
        while not _stop["flag"]:
            try:
                healthy = run_cycle(r, pw)
            except Exception as e:  # noqa: BLE001
                log.exception("цикл упал: %s", e)
                healthy = 0
            if MINE_ONCE:
                break
            # Ни один минт в цикле не удался, а попытки были → снаружи стена.
            # Ускоряться тут нельзя: ретраи её только подогревают. Пауза растёт
            # вдвое за цикл до потолка и сбрасывается первым же успехом.
            if _cycle["mint_ok"] == 0 and _cycle["mint_fail"] > 0:
                wall_cycles += 1
                sleep_min = min(MINE_RETRY_MINUTES * (2 ** (wall_cycles - 1)),
                                MINE_WALL_BACKOFF_MAX_MINUTES)
                log.warning("минт не проходит %d цикл(ов) подряд (похоже на стену) — пауза %.0f мин",
                            wall_cycles, sleep_min)
            else:
                wall_cycles = 0
                # если пул совсем пуст — повторяем быстрее (retry), иначе обычный интервал
                sleep_min = MINE_RETRY_MINUTES if healthy == 0 else CHECK_INTERVAL_MIN
            interruptible_sleep(sleep_min * 60)

    log.info("майнер остановлен")


if __name__ == "__main__":
    main()
