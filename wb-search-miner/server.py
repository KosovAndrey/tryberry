#!/usr/bin/env python3
"""
server.py — wb-search-miner: БРАУЗЕР-КАК-ТРАНСПОРТ для WB-поиска.

Долгоживущий HTTP-сервис с пулом прогретых дорожек (lane). Дорожка = headful
Chromium (patchright, тот же движок, что проходит wbaas в token-miner) в Xvfb.

Зачем: голый direct к `__internal/u-search` с датацентр-IP wbaas режет 403 на
ПОПУЛЯРНЫХ запросах (iphone 17 → 403, капибара → 200), хотя cookie-токен валиден.
Различие — в ТРАНСПОРТЕ (браузер проходит JS/JA3-челлендж, http.Client — нет),
не в IP: майнер с того же IP в браузере ловит 200. Поэтому горячие запросы
уводим в браузер — как у Ozon (ozon-miner) с FAB.

Дорожка прогревается навигацией на страницу поиска нейтрального запроса (проходит
wbaas-стену), затем цену/выдачу достаём «методом друга»: in-page fetch к тому же
u-search ИЗНУТРИ доверенного контекста (cookie + решённый челлендж согласованы).

Go-скрейпер (WildberriesSearchScraper, фолбэк на 403) зовёт
GET /search?query=<q>&sort=<s>&page=<n> → дорожка делает in-page fetch к u-search
и отдаёт СЫРОЙ JSON той же формы, что и direct (wbSearchResponse), зеркаля
upstream-статус (403 при стойкой стене).

Живучесть: джиттер интервала, backoff на стойкой стене (не долбить — жжёт IP),
периодический re-warm, пересоздание браузера после смерти драйвера. Прокси НЕ
нужен (майнер доказал: direct с датацентр-IP проходит), но опционально
поддержан через WB_SEARCH_PROXY_URL / WB_LANE_<i>_PROXY.
"""

import asyncio
import logging
import os
import random
import re
import time
from urllib.parse import quote, unquote, urlparse

from aiohttp import web
from patchright.async_api import async_playwright

# ── Конфиг ───────────────────────────────────────────────────────────────────
PORT = int(os.getenv("WB_SEARCH_MINER_PORT", "8081"))
POOL_SIZE = int(os.getenv("WB_SEARCH_POOL_SIZE", "1"))

WARM_QUERY = os.getenv("WB_WARM_QUERY", "телефон")
SEARCH_PAGE_URL = os.getenv(
    "WB_SEARCH_PAGE_URL",
    "https://www.wildberries.ru/catalog/0/search.aspx?search={query}",
)
# База u-search (должна совпадать с wbSearchAPIBase в Go). Путь берём относительным
# (same-origin) для in-page fetch — хост подставляет сам браузер.
USEARCH_PATH = os.getenv(
    "WB_USEARCH_PATH",
    "/__internal/u-search/exactmatch/ru/common/v18/search",
)
# Фиксированные параметры u-search (совпадают с buildSearchAPIURL в Go).
WB_DEST = os.getenv("WB_DEST", "-1257786")
WB_SPP = os.getenv("WB_SPP", "30")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
# Таймаут одной попытки in-page fetch. Здоровый ответ u-search ~1–2с.
FETCH_TIMEOUT_S = float(os.getenv("WB_FETCH_TIMEOUT_SECONDS", "15"))
FETCH_RETRIES = int(os.getenv("WB_FETCH_RETRIES", "2"))
RETRY_PAUSE_MS = int(os.getenv("WB_RETRY_PAUSE_MS", "1200"))
NAV_TIMEOUT_S = float(os.getenv("WB_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("WB_WARM_WAIT_SECONDS", "150"))
WARM_RELOADS = int(os.getenv("WB_WARM_RELOADS", "2"))
# Маркер u-search в URL ответов (ловим 200 на XHR самой страницы = стена пройдена,
# cookie x_wbaas_token выставлен). Совпадает с путём USEARCH_PATH.
USEARCH_MARKER = os.getenv("WB_USEARCH_MARKER", "/u-search/")

# Человекоподобный интервал между запросами одной дорожки + джиттер.
LANE_MIN_INTERVAL_S = float(os.getenv("WB_LANE_MIN_INTERVAL_MS", "800")) / 1000.0
LANE_JITTER = float(os.getenv("WB_LANE_JITTER", "0.4"))

MAINT_INTERVAL_S = float(os.getenv("WB_HEALTH_INTERVAL_SECONDS", "30"))
WARM_KEEPALIVE_S = float(os.getenv("WB_WARM_KEEPALIVE_MINUTES", "30")) * 60.0
WARM_BACKOFF_MAX_S = float(os.getenv("WB_WARM_BACKOFF_MAX_SECONDS", "600"))

LOCALE = os.getenv("WB_MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("WB_MINER_TIMEZONE", "Europe/Moscow")
WB_USER_AGENT = os.getenv("WB_USER_AGENT", "").strip()
BLOCK_RESOURCES = os.getenv("MINER_BLOCK_RESOURCES", "true").lower() in ("1", "true", "yes")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("wb-search-miner")

WALL_MARKERS = ("почти готов", "подозрительная активность", "что-то не так")
# Признаки смерти драйвера/браузера (транспорт playwright оборвался) — дорожку
# надо пересоздать, а не ре-навигировать труп.
_DEAD_RE = re.compile(
    r"Connection closed|pipe closed|Target (page|frame|browser).*closed|"
    r"Browser.*closed|has been closed|Target closed",
    re.IGNORECASE)


def _is_dead(exc) -> bool:
    return bool(_DEAD_RE.search(str(exc)))


def _first_line(exc) -> str:
    return (str(exc).splitlines() or [""])[0]

# ── In-page fetch к u-search ──────────────────────────────────────────────────
_FETCH_JS = """
async (path) => {
  try {
    const r = await fetch(path, {
      headers: {'accept': '*/*', 'x-requested-with': 'XMLHttpRequest'},
      credentials: 'include',
    });
    const body = await r.text();
    return {status: r.status, body: body};
  } catch (e) { return {status: -1, body: '', error: String(e)}; }
}
"""


def _usearch_path(query: str, sort: str, page: int) -> str:
    """Относительный u-search path (same-origin). Параметры — тот же семантически
    нейтральный минимум, что в Go buildSearchAPIURL; порядок не важен."""
    q = {
        "appType": "1", "curr": "rub", "dest": WB_DEST,
        "inheritFilters": "false", "lang": "ru", "locale": "ru",
        "page": str(page), "query": query, "resultset": "catalog",
        "sort": sort or "popular", "spp": WB_SPP, "suppressSpellcheck": "false",
    }
    qs = "&".join(f"{k}={quote(str(v), safe='')}" for k, v in q.items())
    return f"{USEARCH_PATH}?{qs}"


def _parse_proxy(url: str):
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


def _looks_ok(status: int, body: str) -> bool:
    """200 и тело похоже на JSON выдачи (есть products/data), а не на стену."""
    if status != 200 or not body:
        return False
    head = body[:400].lower()
    if any(m in head for m in WALL_MARKERS):
        return False
    return '"products"' in body[:2000] or '"data"' in body[:2000] or body.lstrip().startswith("{")


# ── Дорожка ───────────────────────────────────────────────────────────────────
class Lane:
    def __init__(self, idx: int, proxy: str):
        self.idx = idx
        self.proxy = proxy
        self.lock = asyncio.Lock()
        self.healthy = False
        self._last_at = 0.0
        self._last_warm = 0.0
        self._warm_fails = 0
        self._next_warm = 0.0
        self._pw = None
        self._browser = None
        self._ctx = None
        self._page = None

    async def _launch(self):
        kw = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
        proxy = _parse_proxy(self.proxy)
        if proxy:
            kw["proxy"] = proxy
        self._browser = await self._pw.chromium.launch(**kw)
        ctx_kw = {"locale": LOCALE, "timezone_id": TIMEZONE, "viewport": {"width": 1366, "height": 768}}
        if WB_USER_AGENT:
            ctx_kw["user_agent"] = WB_USER_AGENT
        self._ctx = await self._browser.new_context(**ctx_kw)
        self._page = await self._ctx.new_page()
        if BLOCK_RESOURCES:
            async def _route(route):
                # Корректный async-хендлер: блокируем тяжёлые статики, остальное
                # (в т.ч. JS челленджа wbaas) пропускаем. Обязательно await —
                # fire-and-forget залипал бы на резолве и рушил загрузку/челлендж.
                try:
                    if route.request.resource_type in ("image", "media", "font"):
                        await route.abort()
                    else:
                        await route.continue_()
                except Exception:  # noqa: BLE001
                    pass
            await self._page.route("**/*", _route)

    async def start(self, pw):
        self._pw = pw
        await self._launch()
        await self.warm()

    async def _relaunch(self) -> bool:
        log.warning("дорожка %d: пересоздаю браузер", self.idx)
        await self.close()
        self._browser = self._ctx = self._page = None
        try:
            await self._launch()
            return True
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: пересоздание упало: %s", self.idx, e)
            return False

    async def _nudge(self):
        try:
            await self._page.mouse.wheel(0, random.randint(200, 900))
        except Exception:  # noqa: BLE001
            pass

    async def _inpage_fetch(self, path: str):
        try:
            res = await asyncio.wait_for(self._page.evaluate(_FETCH_JS, path), timeout=FETCH_TIMEOUT_S)
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: in-page fetch упал: %s", self.idx, str(e).splitlines()[0] if str(e) else e)
            return 0, ""
        return int(res.get("status") or 0), (res.get("body") or "")

    async def warm(self):
        """Прогрев как в token-miner: навигация на страницу поиска, СЛУШАЕМ
        ответы — ждём 200 на СОБСТВЕННОМ u-search XHR страницы (= wbaas-стена
        пройдена, cookie x_wbaas_token выставлен). Потом проверяем in-page fetch.
        Успех → healthy; неудача → экспоненциальный backoff."""
        url = SEARCH_PAGE_URL.format(query=quote(WARM_QUERY))
        seen = {"ok": False, "statuses": [], "all": []}

        def on_resp(resp):
            try:
                seen["all"].append((resp.request.resource_type, resp.status, resp.url))
                if USEARCH_MARKER in resp.url:
                    seen["statuses"].append(resp.status)
                    if resp.status == 200:
                        seen["ok"] = True
            except Exception:  # noqa: BLE001
                pass

        self._page.on("response", on_resp)
        per_attempt = max(30.0, WARM_WAIT_S / max(1, WARM_RELOADS))
        log.info("дорожка %d: прогрев — навигация на %s", self.idx, url)
        try:
            for attempt in range(1, WARM_RELOADS + 1):
                try:
                    await self._page.goto(url, wait_until="domcontentloaded",
                                          timeout=int(NAV_TIMEOUT_S * 1000))
                except Exception as e:  # noqa: BLE001
                    log.warning("дорожка %d: навигация (попытка %d): %s",
                                self.idx, attempt, _first_line(e))
                    if _is_dead(e) and await self._relaunch():
                        self._page.on("response", on_resp)  # новый page — переподписка
                        continue
                end = time.time() + per_attempt
                while time.time() < end and not seen["ok"]:
                    await self._nudge()
                    try:
                        await self._page.wait_for_timeout(2500)
                    except Exception:  # noqa: BLE001
                        await asyncio.sleep(2.5)
                if seen["ok"]:
                    break
                log.info("дорожка %d: попытка %d, 200 нет (u-search: %s)",
                         self.idx, attempt, seen["statuses"][-6:] or "—")
        finally:
            try:
                self._page.remove_listener("response", on_resp)
            except Exception:  # noqa: BLE001
                pass

        # Стена пройдена страницей → убеждаемся, что и наш in-page fetch отдаёт 200.
        if seen["ok"]:
            status, body = await self._inpage_fetch(_usearch_path(WARM_QUERY, "popular", 1))
            if _looks_ok(status, body):
                self.healthy = True
                self._warm_fails = 0
                self._next_warm = 0.0
                self._last_warm = time.monotonic()
                log.info("дорожка %d прогрета: u-search 200 (wbaas пройден)", self.idx)
                return
            log.warning("дорожка %d: страница прошла стену, но in-page fetch дал status=%s",
                        self.idx, status)

        self.healthy = False
        self._warm_fails += 1
        backoff = min(MAINT_INTERVAL_S * (2 ** self._warm_fails), WARM_BACKOFF_MAX_S)
        self._next_warm = time.monotonic() + backoff
        log.warning("дорожка %d: прогрев не дал 200 (подряд %d, u-search: %s) — backoff %.0fс",
                    self.idx, self._warm_fails, seen["statuses"][-6:] or "—", backoff)
        await self._dump_diag(seen)

    async def _dump_diag(self, seen: dict):
        """Диагностика провала прогрева: заголовок/тело страницы + гистограмма
        хостов ответов + ключевые xhr/document — понять, стена это, пусто или
        u-search ушёл на другой URL."""
        try:
            from collections import Counter
            hosts = Counter()
            for (_rt, _st, u) in seen["all"]:
                try:
                    hosts[urlparse(u).netloc] += 1
                except Exception:  # noqa: BLE001
                    pass
            log.warning("─── ДИАГ дорожка %d ───", self.idx)
            log.warning("ответов всего: %d, хосты: %s", len(seen["all"]), dict(hosts.most_common(8)))
            for rt, st, u in seen["all"]:
                if rt in ("document", "xhr", "fetch"):
                    pu = urlparse(u)
                    log.warning("  %-8s %s  %s%s", rt, st, pu.netloc, pu.path[:80])
            try:
                title = await self._page.title()
                body = await self._page.evaluate("() => document.body ? document.body.innerText : ''")
                log.warning("url=%s title=%r", self._page.url, title)
                log.warning("body[:300]=%r", (body or "")[:300].replace("\n", " "))
            except Exception as e:  # noqa: BLE001
                log.warning("тело недоступно: %s", _first_line(e))
            log.warning("─── /ДИАГ ───")
        except Exception as e:  # noqa: BLE001
            log.warning("диагностика упала: %s", _first_line(e))

    async def fetch_search(self, query: str, sort: str, page: int):
        """In-page fetch страницы выдачи. Возвращает (status, body_bytes).
        Стойкая стена (403/тело-стена) НЕ лечится ретраем в пределах вызова —
        метим дорожку нездоровой (перепрогрев сделает maintenance_loop)."""
        path = _usearch_path(query, sort, page)
        async with self.lock:
            t0 = time.monotonic()
            spacing = max(0.1, LANE_MIN_INTERVAL_S * (1.0 + LANE_JITTER * (2 * random.random() - 1)))
            wait = spacing - (t0 - self._last_at)
            if wait > 0:
                await asyncio.sleep(wait)
            status, body = 0, ""
            for attempt in range(1, FETCH_RETRIES + 1):
                self._last_at = time.monotonic()
                status, body = await self._inpage_fetch(path)
                if _looks_ok(status, body):
                    log.info("дорожка %d: search %r p%d ок (%d симв., попыток %d)",
                             self.idx, query[:40], page, len(body), attempt)
                    return 200, body.encode("utf-8")
                if status == 403 or any(m in (body[:400].lower()) for m in WALL_MARKERS):
                    break  # стена — ретрай бесполезен
                if attempt < FETCH_RETRIES:
                    await self._nudge()
                    try:
                        await self._page.wait_for_timeout(RETRY_PAUSE_MS)
                    except Exception:  # noqa: BLE001
                        await asyncio.sleep(RETRY_PAUSE_MS / 1000)
            self.healthy = False
            log.warning("дорожка %d: стена на search %r p%d (status=%s) — нездорова",
                        self.idx, query[:40], page, status)
            return 403, (body or "").encode("utf-8")

    def due_keepalive(self, now: float) -> bool:
        return self.healthy and WARM_KEEPALIVE_S > 0 and (now - self._last_warm) >= WARM_KEEPALIVE_S

    def due_rewarm(self, now: float) -> bool:
        return (not self.healthy) and now >= self._next_warm

    async def close(self):
        for obj in (self._ctx, self._browser):
            try:
                if obj:
                    await obj.close()
            except Exception:  # noqa: BLE001
                pass


# ── Пул ────────────────────────────────────────────────────────────────────────
class Pool:
    def __init__(self, lanes):
        self.lanes = lanes

    def pick(self):
        alive = [l for l in self.lanes if l.healthy]
        return random.choice(alive) if alive else None

    def healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy)

    async def maintenance_loop(self):
        while True:
            await asyncio.sleep(MAINT_INTERVAL_S)
            now = time.monotonic()
            for lane in self.lanes:
                if lane.lock.locked():
                    continue
                try:
                    if lane.due_rewarm(now):
                        async with lane.lock:
                            log.info("дорожка %d нездорова — перепрогрев", lane.idx)
                            await lane.warm()
                    elif lane.due_keepalive(now):
                        async with lane.lock:
                            log.info("дорожка %d: keepalive-прогрев", lane.idx)
                            await lane.warm()
                except Exception as e:  # noqa: BLE001
                    log.error("дорожка %d: обслуживание упало: %s", lane.idx, e)


# ── HTTP ────────────────────────────────────────────────────────────────────────
async def handle_search(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    query = (request.query.get("query") or "").strip()
    if not query:
        return web.json_response({"error": "query required"}, status=400)
    sort = (request.query.get("sort") or "popular").strip()
    try:
        page = max(1, int(request.query.get("page") or "1"))
    except ValueError:
        page = 1
    lane = pool.pick()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_search(query, sort, page)
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body, content_type="application/json",
                        headers={"X-WB-Lane": str(lane.idx)})


async def handle_health(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    healthy = pool.healthy_count()
    return web.json_response(
        {"healthy": healthy, "total": len(pool.lanes),
         "lanes": [{"idx": l.idx, "healthy": l.healthy} for l in pool.lanes]},
        status=200 if healthy > 0 else 503)


async def handle_metrics(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    lines = [
        "# HELP wb_search_miner_healthy_lanes Число прогретых дорожек",
        "# TYPE wb_search_miner_healthy_lanes gauge",
        f"wb_search_miner_healthy_lanes {pool.healthy_count()}",
        "# HELP wb_search_miner_total_lanes Всего дорожек",
        "# TYPE wb_search_miner_total_lanes gauge",
        f"wb_search_miner_total_lanes {len(pool.lanes)}",
        "# HELP wb_search_miner_lane_healthy Здоровье дорожки (1/0)",
        "# TYPE wb_search_miner_lane_healthy gauge",
    ]
    for l in pool.lanes:
        lines.append(f'wb_search_miner_lane_healthy{{lane="{l.idx}"}} {1 if l.healthy else 0}')
    return web.Response(text="\n".join(lines) + "\n", content_type="text/plain")


def _lane_proxies():
    out = []
    for i in range(POOL_SIZE):
        p = os.getenv(f"WB_LANE_{i}_PROXY", "").strip()
        if i == 0:
            p = p or os.getenv("WB_SEARCH_PROXY_URL", "").strip()
        out.append(p)
    return out


async def main():
    proxies = _lane_proxies()
    log.info("старт wb-search-miner: port=%d дорожек=%d warm=%r headless=%s proxy=%s",
             PORT, POOL_SIZE, WARM_QUERY, HEADLESS, any(proxies))

    async with async_playwright() as pw:
        lanes = []
        for i in range(POOL_SIZE):
            lane = Lane(i, proxies[i])
            try:
                await lane.start(pw)
            except Exception as e:  # noqa: BLE001
                log.error("дорожка %d: старт упал: %s", i, e)
            lanes.append(lane)
        pool = Pool(lanes)

        app = web.Application()
        app["pool"] = pool
        app.router.add_get("/search", handle_search)
        app.router.add_get("/healthz", handle_health)
        app.router.add_get("/metrics", handle_metrics)

        asyncio.ensure_future(pool.maintenance_loop())

        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "0.0.0.0", PORT)
        await site.start()
        log.info("слушаю :%d (живых дорожек %d/%d)", PORT, pool.healthy_count(), len(lanes))

        while True:
            await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())
