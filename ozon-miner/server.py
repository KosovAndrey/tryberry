#!/usr/bin/env python3
"""
server.py — ozon-miner, фаза 2: БРАУЗЕР-КАК-ТРАНСПОРТ с ПУЛОМ ДОРОЖЕК (camoufox).

Долгоживущий HTTP-сервис. Держит пул из N «дорожек» (lane). Каждая дорожка =
отдельный залогиненный браузер **camoufox** (анти-детект Firefox) через СВОЙ
мобильный прокси и СВОЮ аккаунт-cookie. Живая сессия проходит антибот FAB
(camoufox пробил его там, где голый Chromium палился) и сама держит доверие.
Цену достаём «методом друга»: in-page fetch к entrypoint-api ИЗНУТРИ доверенного
контекста (JA3 + куки + решённый челлендж согласованы) — см. probe.py.

Go-скрейпер в browser-режиме зовёт GET /scrape?id=<id> — сервис маршрутизирует на
дорожку (аффинити по id), делает in-page fetch и отдаёт СЫРОЙ widgetStates,
зеркаля upstream-статус Ozon (403 при FAB). Go разбирает тем же parseOzonWidgets.

Масштабирование = добавить дорожек (по IP+аккаунту):
  OZON_POOL_SIZE=3
  OZON_LANE_0_PROXY/_COOKIE, OZON_LANE_1_PROXY/_COOKIE, ...
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
from camoufox.async_api import AsyncCamoufox

# ── Конфиг ───────────────────────────────────────────────────────────────────
PORT = int(os.getenv("OZON_MINER_PORT", "8080"))
POOL_SIZE = int(os.getenv("OZON_POOL_SIZE", "1"))

# Прогрев: главная Ozon — задаёт origin www.ozon.ru и поднимает доверие FAB.
WARM_URL = os.getenv("OZON_WARM_URL", "https://www.ozon.ru/")
# Тестовый товар для прогрева (проверяем, что FAB пройден до пометки healthy).
WARM_PRODUCT_ID = os.getenv("OZON_WARM_PRODUCT_ID", "1889984997")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
# Человекоподобный минимум между запросами одной дорожки.
LANE_MIN_INTERVAL_S = float(os.getenv("OZON_LANE_MIN_INTERVAL_MS", "1500")) / 1000.0
SCRAPE_TIMEOUT_S = float(os.getenv("OZON_SCRAPE_TIMEOUT_SECONDS", "30"))
SCRAPE_RETRIES = int(os.getenv("OZON_SCRAPE_RETRIES", "3"))
NAV_TIMEOUT_S = float(os.getenv("OZON_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("OZON_WARM_WAIT_SECONDS", "45"))
# Фоновый чинильщик дорожек (перепрогрев нездоровых).
HEALTH_INTERVAL_S = float(os.getenv("OZON_HEALTH_INTERVAL_SECONDS", "30"))
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-miner")

_FAB_RE = re.compile(r"fab_|incidentId")

# In-page fetch к entrypoint-api ИЗНУТРИ доверенного контекста. Возвращает сырое
# тело widgetStates (Go разбирает его) + статус.
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
    return {status: r.status, body: body};
  } catch (e) { return {status: -1, body: '', error: String(e)}; }
}
"""


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


def _cookie_jar(header: str):
    """'k=v; k2=v2' → add_cookies на .ozon.ru. secure=True ОБЯЗАТЕЛЕН: иначе
    браузер отвергает всю пачку из-за куки __Secure-/__Host-."""
    out = []
    for part in header.split(";"):
        if "=" in part:
            k, v = part.strip().split("=", 1)
            if k.strip():
                out.append({"name": k.strip(), "value": v.strip(),
                            "domain": ".ozon.ru", "path": "/", "secure": True})
    return out


async def _add_cookies_safe(context, cookies) -> int:
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
    """Залогиненная camoufox-сессия через свой прокси. Сериализует запросы и
    держит человекоподобный интервал. Прогревается до healthy, проверяя что FAB
    реально пройден (тестовый in-page fetch отдаёт 200)."""

    def __init__(self, cfg: dict):
        self.idx = cfg["idx"]
        self.proxy = cfg["proxy"]
        self.cookie = cfg["cookie"]
        self.lock = asyncio.Lock()
        self.healthy = False
        self.egress_ip = ""
        self._last_at = 0.0
        self._cam = None
        self._browser = None
        self._page = None

    async def start(self):
        kw = {"headless": HEADLESS}
        proxy = _parse_proxy(self.proxy)
        if proxy:
            kw["proxy"] = proxy
        # geoip=True рекомендуется camoufox при прокси (выравнивает локаль/таймзону/гео
        # под exit-IP, чтобы не палиться). Включим, если установлен extra; иначе без.
        try:
            self._cam = AsyncCamoufox(geoip=True, **kw)
            self._browser = await self._cam.__aenter__()
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: geoip недоступен (%s) — запускаю без него",
                        self.idx, str(e).splitlines()[0])
            self._cam = AsyncCamoufox(**kw)
            self._browser = await self._cam.__aenter__()
        self._page = await self._browser.new_page()
        await _add_cookies_safe(self._page.context, _cookie_jar(self.cookie))
        await self.warm()

    async def warm(self):
        """Навигация на главную + ожидание, что FAB пройден (тестовый fetch=200).
        Только тогда healthy=True."""
        try:
            await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                  timeout=int(NAV_TIMEOUT_S * 1000))
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: навигация прогрева: %s",
                        self.idx, str(e).splitlines()[0])
        self.egress_ip = await self._egress_ip()
        # ждём, пока сессия станет доверенной (FAB решится) — как в probe.py
        deadline = time.time() + WARM_WAIT_S
        while time.time() < deadline:
            status, body = await self._inpage_fetch(WARM_PRODUCT_ID)
            if status == 200 and not _looks_blocked(status, body):
                self.healthy = True
                log.info("дорожка %d прогрета: egress=%s, FAB пройден", self.idx, self.egress_ip or "?")
                return
            await self._nudge()
            await self._page.wait_for_timeout(3000)
        self.healthy = False
        log.warning("дорожка %d: прогрев не дал 200 за %.0fс (FAB не пройден)",
                    self.idx, WARM_WAIT_S)

    async def scrape(self, product_id: str):
        """In-page fetch карточки с ретраями. На стойкий FAB метит дорожку
        нездоровой (фоновый чинильщик перепрогреет). Возвращает (status, body_bytes)."""
        async with self.lock:
            wait = LANE_MIN_INTERVAL_S - (time.monotonic() - self._last_at)
            if wait > 0:
                await asyncio.sleep(wait)
            for attempt in range(1, SCRAPE_RETRIES + 1):
                self._last_at = time.monotonic()
                status, body = await self._inpage_fetch(product_id)
                if status == 200 and not _looks_blocked(status, body):
                    return 200, body.encode("utf-8")
                if attempt < SCRAPE_RETRIES:
                    await self._nudge()
                    await self._page.wait_for_timeout(2000)
            # не пробились — метим нездоровой и отдаём 403 (Go → blocked, как upstream)
            self.healthy = False
            log.warning("дорожка %d: FAB/блок на id=%s (status=%s) — перепрогрев",
                        self.idx, product_id, status)
            return 403, (body or "").encode("utf-8")

    async def _inpage_fetch(self, product_id: str):
        try:
            res = await asyncio.wait_for(
                self._page.evaluate(_FETCH_JS, product_id), timeout=SCRAPE_TIMEOUT_S)
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: fetch упал: %s", self.idx, str(e).splitlines()[0])
            return 0, ""
        return int(res.get("status") or 0), (res.get("body") or "")

    async def _nudge(self):
        try:
            await self._page.mouse.wheel(0, random.randint(200, 900))
        except Exception:  # noqa: BLE001
            pass

    async def _egress_ip(self) -> str:
        try:
            p = await self._browser.new_page()
            await p.goto("https://api.ipify.org?format=json", timeout=15000)
            txt = await p.evaluate("() => document.body.innerText")
            await p.close()
            m = re.search(r'"ip":\s*"([^"]+)"', txt or "")
            return m.group(1) if m else ""
        except Exception:  # noqa: BLE001
            return ""

    async def close(self):
        try:
            if self._cam:
                await self._cam.__aexit__(None, None, None)
        except Exception:  # noqa: BLE001
            pass


# ── Пул ──────────────────────────────────────────────────────────────────────
class Pool:
    def __init__(self, lanes):
        self.lanes = lanes

    def pick(self, product_id: str):
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
    return web.json_response({"healthy": healthy, "total": len(pool.lanes), "lanes": lanes},
                             status=200 if healthy > 0 else 503)


async def main():
    configs = _load_lane_configs()
    if not configs:
        raise SystemExit("нет ни одной сконфигурённой дорожки: задай OZON_COOKIE "
                         "(дорожка 0) или OZON_LANE_<i>_COOKIE")
    log.info("старт ozon-miner: port=%d дорожек=%d (POOL_SIZE=%d) движок=camoufox",
             PORT, len(configs), POOL_SIZE)

    lanes = []
    for cfg in configs:
        lane = Lane(cfg)
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

    while True:
        await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())
