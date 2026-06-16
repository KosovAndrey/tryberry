#!/usr/bin/env python3
"""
server.py — ozon-miner, фаза 2: БРАУЗЕР-КАК-ТРАНСПОРТ с ПУЛОМ ДОРОЖЕК.

Долгоживущий HTTP-сервис. Держит пул из N «дорожек» (lane). Каждая дорожка =
отдельный залогиненный Chromium (Patchright, стелс-форк Playwright) через СВОЙ
мобильный прокси и СВОЮ аккаунт-cookie. Живая сессия дорожки проходит антибот FAB
и сама рефрешит access-token (это делает веб-приложение Ozon в фоне). Цену достаём
«методом друга»: in-page fetch к entrypoint-api ИЗНУТРИ доверенного контекста —
JA3 + куки + решённый челлендж остаются согласованными (см. probe.py, фаза 1).

Go-скрейпер в browser-режиме зовёт:

    GET /scrape?id=<product_id>

сервис маршрутизирует запрос на дорожку (аффинити по id → стабильная сессия),
делает in-page fetch и отдаёт СЫРОЙ widgetStates, зеркаля upstream-статус Ozon
(в т.ч. 403 при FAB). Дальше Go разбирает тело тем же parseOzonWidgets.

── Масштабирование ───────────────────────────────────────────────────────────
Узкое место — НЕ браузер, а связка {мобильный IP + аккаунт}. Растём, добавляя
дорожки (каждая = свой IP + аккаунт + браузер):

    OZON_POOL_SIZE=3
    OZON_LANE_0_PROXY=http://user:pass@host1:port   OZON_LANE_0_COOKIE=...
    OZON_LANE_1_PROXY=http://user:pass@host2:port   OZON_LANE_1_COOKIE=...
    OZON_LANE_2_PROXY=http://user:pass@host3:port   OZON_LANE_2_COOKIE=...

Дорожка 0 фолбэчит на legacy OZON_PROXY_URL / OZON_COOKIE (старт N=1 без правок).
"""

import asyncio
import logging
import os
import random
import re
import time
from urllib.parse import unquote, urlparse

from aiohttp import web
from patchright.async_api import async_playwright

# ── Конфиг из окружения ──────────────────────────────────────────────────────
PORT = int(os.getenv("OZON_MINER_PORT", "8080"))
POOL_SIZE = int(os.getenv("OZON_POOL_SIZE", "1"))

# Прогрев/база сессии: главная Ozon (там приложение поднимает куки и проходит FAB).
WARM_URL = os.getenv("OZON_WARM_URL", "https://www.ozon.ru/")
# Эндпоинт карточки (web): отдаёт widgetStates, как и mobile composer-api.
PRODUCT_API = "/api/entrypoint-api.bx/page/json/v2?url=" + "%2Fproduct%2F{id}%2F"

# Человекоподобный минимум между запросами одной дорожки (один IP/аккаунт = бюджет
# одного живого юзера). Тюним по спайку «сколько req/мин до челленджа».
LANE_MIN_INTERVAL_S = float(os.getenv("OZON_LANE_MIN_INTERVAL_MS", "1500")) / 1000.0
SCRAPE_TIMEOUT_S = float(os.getenv("OZON_SCRAPE_TIMEOUT_SECONDS", "30"))
WARM_TIMEOUT_S = float(os.getenv("OZON_WARM_TIMEOUT_SECONDS", "45"))
# Фоновый чинильщик дорожек (перепрогрев нездоровых), как «ремонтник» в WB-майнере.
HEALTH_INTERVAL_S = float(os.getenv("OZON_HEALTH_INTERVAL_SECONDS", "30"))

BLOCK_RESOURCES = os.getenv("MINER_BLOCK_RESOURCES", "true").lower() in ("1", "true", "yes")
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
LOCALE = os.getenv("MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("MINER_TIMEZONE", "Europe/Moscow")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-miner")

# Маркеры FAB в теле ответа (best-effort; спайк уточнит реальную сигнатуру).
_FAB_RE = re.compile(r"fab_|incidentId")

# In-page fetch «методом друга»: запрос к entrypoint-api ИЗНУТРИ доверенного
# контекста страницы. credentials:'include' тащит куки сессии, fetch едет тем же
# JA3, что и прошедший FAB браузер.
_FETCH_JS = """
async (id) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' +
              encodeURIComponent('/product/' + id + '/');
  try {
    const r = await fetch(url, {headers: {accept: 'application/json'},
                                credentials: 'include'});
    const body = await r.text();
    return {status: r.status, body: body};
  } catch (e) { return {status: -1, body: '', error: String(e)}; }
}
"""


def _parse_proxy(url: str):
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


def _cookie_jar(header: str):
    """'k=v; k2=v2' → формат Playwright add_cookies (домен .ozon.ru). secure=True
    ОБЯЗАТЕЛЕН: иначе Chrome отвергает всю пачку из-за куки __Secure-/__Host-."""
    out = []
    for part in header.split(";"):
        if "=" in part:
            k, v = part.strip().split("=", 1)
            if k.strip():
                out.append({"name": k.strip(), "value": v.strip(),
                            "domain": ".ozon.ru", "path": "/", "secure": True})
    return out


async def _add_cookies_safe(context, cookies) -> int:
    """Добавить куки устойчиво: пачкой, при отказе — по-одной, пропуская кривые
    (в строке из приложения бывают не-cookie поля вроде x-o3-* со скобками)."""
    try:
        await context.add_cookies(cookies)
        return len(cookies)
    except Exception:  # noqa: BLE001
        ok = 0
        for c in cookies:
            try:
                await context.add_cookies([c])
                ok += 1
            except Exception as e:  # noqa: BLE001
                log.warning("дорожка: пропускаю cookie %r: %s",
                            c["name"], str(e).splitlines()[0])
        return ok


def _looks_blocked(status: int, body: str) -> bool:
    return status == 403 or bool(_FAB_RE.search(body or ""))


def _load_lane_configs():
    """Собирает конфиги дорожек из env. Дорожка i: OZON_LANE_<i>_PROXY/_COOKIE;
    дорожка 0 фолбэчит на legacy OZON_PROXY_URL / OZON_COOKIE."""
    configs = []
    for i in range(POOL_SIZE):
        proxy = os.getenv(f"OZON_LANE_{i}_PROXY", "").strip()
        cookie = os.getenv(f"OZON_LANE_{i}_COOKIE", "").strip()
        if i == 0:
            proxy = proxy or os.getenv("OZON_PROXY_URL", "").strip()
            cookie = cookie or os.getenv("OZON_COOKIE", "").strip()
        if not cookie:
            log.warning("дорожка %d: нет cookie (OZON_LANE_%d_COOKIE) — пропускаю", i, i)
            continue
        configs.append({"idx": i, "proxy": proxy, "cookie": cookie})
    return configs


# ── Дорожка (lane) ───────────────────────────────────────────────────────────
class Lane:
    """Одна залогиненная браузер-сессия через свой прокси. Сериализует запросы
    (один in-page fetch за раз) и держит человекоподобный интервал."""

    def __init__(self, pw, cfg: dict):
        self._pw = pw
        self.idx = cfg["idx"]
        self.proxy = cfg["proxy"]
        self.cookie = cfg["cookie"]
        self.lock = asyncio.Lock()
        self.healthy = False
        self.egress_ip = ""
        self._last_at = 0.0
        self._browser = None
        self._context = None
        self._page = None

    async def start(self):
        launch = {"headless": HEADLESS, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
        proxy = _parse_proxy(self.proxy)
        if proxy:
            launch["proxy"] = proxy
        self._browser = await self._pw.chromium.launch(**launch)
        self._context = await self._browser.new_context(
            locale=LOCALE, timezone_id=TIMEZONE, viewport={"width": 1366, "height": 768})
        await _add_cookies_safe(self._context, _cookie_jar(self.cookie))
        self._page = await self._context.new_page()
        if BLOCK_RESOURCES:
            async def _block(route):
                if route.request.resource_type in ("image", "media", "font"):
                    await route.abort()
                else:
                    await route.continue_()
            await self._page.route("**/*", _block)
        await self.warm()

    async def warm(self):
        """Прогрев: навигация на главную (поднять/освежить сессию, пройти FAB).
        Успех → healthy=True. TODO(спайк): уточнить маркер «сессия жива»."""
        try:
            await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                  timeout=int(WARM_TIMEOUT_S * 1000))
            await self._page.wait_for_timeout(random.randint(1500, 3000))
            self.egress_ip = await self._egress_ip()
            self.healthy = True
            log.info("дорожка %d прогрета: egress=%s", self.idx, self.egress_ip or "?")
        except Exception as e:  # noqa: BLE001
            self.healthy = False
            log.warning("дорожка %d: прогрев не удался: %s", self.idx, e)

    async def scrape(self, product_id: str):
        """In-page fetch карточки. Возвращает (status, body_bytes). На FAB метит
        дорожку нездоровой (фоновый чинильщик перепрогреет)."""
        async with self.lock:
            # человекоподобный интервал между запросами одной сессии
            wait = LANE_MIN_INTERVAL_S - (time.monotonic() - self._last_at)
            if wait > 0:
                await asyncio.sleep(wait)
            self._last_at = time.monotonic()
            try:
                res = await asyncio.wait_for(
                    self._page.evaluate(_FETCH_JS, product_id), timeout=SCRAPE_TIMEOUT_S)
            except Exception as e:  # noqa: BLE001
                self.healthy = False
                log.warning("дорожка %d: fetch упал (%s) — метим нездоровой", self.idx, e)
                return 0, b""
            status = int(res.get("status") or 0)
            body = (res.get("body") or "")
            if _looks_blocked(status, body):
                self.healthy = False
                inc = _FAB_RE.search(body)
                log.warning("дорожка %d: FAB/блок (status=%s, marker=%s) — перепрогрев",
                            self.idx, status, inc.group(0) if inc else "?")
                # отдаём 403, чтобы Go классифицировал как blocked (как upstream)
                return 403, body.encode("utf-8")
            return status, body.encode("utf-8")

    async def _egress_ip(self) -> str:
        try:
            p = await self._context.new_page()
            await p.goto("https://api.ipify.org?format=json", timeout=15000)
            txt = await p.evaluate("() => document.body.innerText")
            await p.close()
            m = re.search(r'"ip":\s*"([^"]+)"', txt or "")
            return m.group(1) if m else ""
        except Exception:  # noqa: BLE001
            return ""

    async def close(self):
        try:
            if self._browser:
                await self._browser.close()
        except Exception:  # noqa: BLE001
            pass


# ── Пул ──────────────────────────────────────────────────────────────────────
class Pool:
    def __init__(self, lanes):
        self.lanes = lanes

    def pick(self, product_id: str):
        """Аффинити по id → стабильная сессия на товар. Если выбранная дорожка
        нездорова — фолбэк на любую живую."""
        if not self.lanes:
            return None
        try:
            i = int(product_id) % len(self.lanes)
        except ValueError:
            i = hash(product_id) % len(self.lanes)
        lane = self.lanes[i]
        if lane.healthy:
            return lane
        alive = [l for l in self.lanes if l.healthy]
        return alive[i % len(alive)] if alive else None

    def healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy)

    async def repair_loop(self):
        """Фоновый чинильщик: перепрогревает нездоровые дорожки (аналог
        ремонтника пула в WB-майнере)."""
        while True:
            await asyncio.sleep(HEALTH_INTERVAL_S)
            for lane in self.lanes:
                if not lane.healthy and not lane.lock.locked():
                    log.info("дорожка %d нездорова — перепрогреваю", lane.idx)
                    async with lane.lock:
                        await lane.warm()


# ── HTTP ─────────────────────────────────────────────────────────────────────
async def handle_scrape(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    product_id = (request.query.get("id") or "").strip()
    if not product_id.isdigit():
        return web.json_response({"error": "id must be numeric"}, status=400)
    lane = pool.pick(product_id)
    if lane is None:
        # нет живых дорожек → 502, Go поймёт как «сайдкар недоступен» (не FAB)
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.scrape(product_id)
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body,
                        content_type="application/json",
                        headers={"X-Ozon-Lane": str(lane.idx)})


async def handle_health(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    lanes = [{"idx": l.idx, "healthy": l.healthy, "egress_ip": l.egress_ip}
             for l in pool.lanes]
    healthy = pool.healthy_count()
    return web.json_response(
        {"healthy": healthy, "total": len(pool.lanes), "lanes": lanes},
        status=200 if healthy > 0 else 503)


async def main():
    configs = _load_lane_configs()
    if not configs:
        raise SystemExit("нет ни одной сконфигурённой дорожки: задай OZON_COOKIE "
                         "(дорожка 0) или OZON_LANE_<i>_COOKIE")
    log.info("старт ozon-miner: port=%d дорожек=%d (из POOL_SIZE=%d) interval=%.1fс",
             PORT, len(configs), POOL_SIZE, LANE_MIN_INTERVAL_S)

    pw = await async_playwright().start()
    lanes = []
    for cfg in configs:
        lane = Lane(pw, cfg)
        try:
            await lane.start()
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: старт упал: %s", cfg["idx"], e)
        lanes.append(lane)
    pool = Pool(lanes)

    app = web.Application()
    app["pool"] = pool
    app.router.add_get("/scrape", handle_scrape)
    app.router.add_get("/healthz", handle_health)

    asyncio.ensure_future(pool.repair_loop())

    runner = web.AppRunner(app)
    await runner.setup()
    site = web.TCPSite(runner, "0.0.0.0", PORT)
    await site.start()
    log.info("слушаю :%d (живых дорожек %d/%d)", PORT, pool.healthy_count(), len(lanes))

    # держим процесс
    while True:
        await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())
