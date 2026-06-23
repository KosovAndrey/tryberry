#!/usr/bin/env python3
"""
server.py — ozon-miner, фаза 2: БРАУЗЕР-КАК-ТРАНСПОРТ с ПУЛОМ ДОРОЖЕК (camoufox).

Долгоживущий HTTP-сервис. Держит пул из N «дорожек» (lane). Каждая дорожка =
отдельный залогиненный браузер **camoufox** (анти-детект Firefox) через СВОЙ
мобильный прокси и СВОЮ аккаунт-cookie. Живая сессия проходит антибот FAB
(camoufox пробил его там, где голый Chromium палился) и сама держит доверие/токен.
Цену достаём «методом друга»: in-page fetch к entrypoint-api ИЗНУТРИ доверенного
контекста (JA3 + куки + решённый челлендж согласованы) — см. probe.py.

Go-скрейпер (browser-режим) зовёт GET /scrape?id=<id> (карточка) или
GET /search?text=<запрос> (выдача) → дорожка делает in-page fetch к тому же
универсальному entrypoint-api и отдаёт СЫРОЙ widgetStates, зеркаля upstream-статус
(403 при FAB).

ЗАЩИТА ОТ БАНА (живучесть):
  - джиттер интервала между запросами (не ровный паттерн);
  - backoff при стойком FAB (НЕ долбить — ретрай-шторм жжёт IP);
  - периодический re-warm живой сессии (рефреш токена/доверия);
  - опц. ротация IP по switch-ссылке провайдера (OZON_PROXY_ROTATE_URL) с
    последующим пере-прогревом — против накопления репутации на одном IP.

Масштабирование = добавить дорожек (по IP+аккаунту): OZON_POOL_SIZE +
OZON_LANE_<i>_PROXY/_COOKIE. Дорожка 0 фолбэчит на OZON_PROXY_URL / OZON_COOKIE.
"""

import asyncio
import logging
import os
import random
import re
import time
from urllib.parse import unquote, urlparse

import aiohttp
from aiohttp import web
from camoufox.async_api import AsyncCamoufox

# ── Конфиг ───────────────────────────────────────────────────────────────────
PORT = int(os.getenv("OZON_MINER_PORT", "8080"))
POOL_SIZE = int(os.getenv("OZON_POOL_SIZE", "1"))

# Прогрев идёт на СТРАНИЦУ ТОВАРА (проверенный пробой путь), а не на тяжёлую
# главную: FAB гейтит главную иначе и навигация залипает.
WARM_PRODUCT_ID = os.getenv("OZON_WARM_PRODUCT_ID", "1889984997")
WARM_URL = os.getenv("OZON_WARM_URL", f"https://www.ozon.ru/product/{WARM_PRODUCT_ID}/")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
# Таймаут ОДНОЙ попытки in-page fetch. Здоровый ответ ~1.7с, поэтому 12с — с большим
# запасом; держим низким, чтобы залипшая попытка не растягивала latency (старые 30с
# давали хвост p99=30с). Худший случай вызова ≈ SCRAPE_TIMEOUT_S*RETRIES + паузы.
SCRAPE_TIMEOUT_S = float(os.getenv("OZON_SCRAPE_TIMEOUT_SECONDS", "12"))
# Ретраи — только для ТРАНЗИЕНТНЫХ пустышек (таймаут/0). FAB-блок ретраем не лечится
# (см. fetch_path), поэтому 2 попытки достаточно.
SCRAPE_RETRIES = int(os.getenv("OZON_SCRAPE_RETRIES", "2"))
# Пауза между транзиентными попытками (нудж + дать странице осесть).
RETRY_PAUSE_MS = int(os.getenv("OZON_RETRY_PAUSE_MS", "1200"))
NAV_TIMEOUT_S = float(os.getenv("OZON_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("OZON_WARM_WAIT_SECONDS", "45"))

# Человекоподобный интервал между запросами одной дорожки + джиттер (±доля).
LANE_MIN_INTERVAL_S = float(os.getenv("OZON_LANE_MIN_INTERVAL_MS", "1500")) / 1000.0
LANE_JITTER = float(os.getenv("OZON_LANE_JITTER", "0.4"))  # ±40%

# Шаг обслуживающего цикла (проверка здоровья/ротации/keepalive).
MAINT_INTERVAL_S = float(os.getenv("OZON_HEALTH_INTERVAL_SECONDS", "30"))
# Периодический re-warm живой дорожки (рефреш сессии/токена). 0 → выкл.
WARM_KEEPALIVE_S = float(os.getenv("OZON_WARM_KEEPALIVE_MINUTES", "45")) * 60.0
# Потолок backoff при неудачных прогревах (не долбить FAB).
WARM_BACKOFF_MAX_S = float(os.getenv("OZON_WARM_BACKOFF_MAX_SECONDS", "600"))

# Ротация IP по switch-ссылке провайдера. Пусто → ротация выкл.
ROTATE_URL = os.getenv("OZON_PROXY_ROTATE_URL", "").strip()
# Как часто ротировать (минуты). 0 → выкл. Частую ставить можно, но каждая
# ротация = пере-прогрев (заново пройти FAB на новом IP, дорожка ~минуту занята).
ROTATE_INTERVAL_S = float(os.getenv("OZON_ROTATE_INTERVAL_MINUTES", "0")) * 60.0
# Пауза после дёрганья switch-ссылки, чтобы прокси успел сменить exit-IP.
ROTATE_SETTLE_S = float(os.getenv("OZON_ROTATE_SETTLE_SECONDS", "6"))

LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ozon-miner")

_FAB_RE = re.compile(r"fab_|incidentId")
# Признаки СМЕРТИ драйвера/браузера (playwright-транспорт оборвался): дорожка
# работает на трупе и без пересоздания camoufox будет вечно крутиться в ошибках
# ('Connection closed while reading from the driver', 'pipe closed by peer',
# 'Target closed' и т.п.). См. Lane._relaunch.
_DEAD_RE = re.compile(
    r"Connection closed|pipe closed|Target (page|frame|browser).*closed|"
    r"Browser.*closed|has been closed|Target closed|Navigation failed because browser",
    re.IGNORECASE)


def _is_dead(exc) -> bool:
    return bool(_DEAD_RE.search(str(exc)))


def _first_line(exc) -> str:
    """Первая строка текста исключения. str(exc).splitlines()[0] падал с
    IndexError, когда у исключения пустой текст (splitlines() → []) — и ронял
    обработчик ошибки вместо логирования."""
    return (str(exc).splitlines() or [""])[0]

# ── Гистограмма латентности скрейпа (без prometheus_client) ───────────────────
# Меряет латентность ВНУТРИ сайдкара (in-page fetch + спейсинг + ретраи) по метке
# outcome=ok|blocked — раньше латентность была видна только тоталом на Go-стороне,
# без разбивки. asyncio однопоточен → инкременты без локов безопасны.
_LAT_BUCKETS = [0.5, 1, 2, 5, 10, 30]
_lat = {
    "ok": {"counts": [0] * (len(_LAT_BUCKETS) + 1), "sum": 0.0},
    "blocked": {"counts": [0] * (len(_LAT_BUCKETS) + 1), "sum": 0.0},
}


def _observe_latency(outcome: str, sec: float):
    h = _lat.get(outcome)
    if h is None:
        return
    h["sum"] += sec
    for i, b in enumerate(_LAT_BUCKETS):
        if sec <= b:
            h["counts"][i] += 1
            return
    h["counts"][-1] += 1  # +Inf


def _latency_metric_lines():
    lines = [
        "# HELP ozon_miner_scrape_duration_seconds Латентность скрейпа внутри сайдкара",
        "# TYPE ozon_miner_scrape_duration_seconds histogram",
    ]
    for outcome, h in _lat.items():
        cum = 0
        for i, b in enumerate(_LAT_BUCKETS):
            cum += h["counts"][i]
            lines.append(
                f'ozon_miner_scrape_duration_seconds_bucket{{outcome="{outcome}",le="{b}"}} {cum}')
        total = cum + h["counts"][-1]
        lines.append(
            f'ozon_miner_scrape_duration_seconds_bucket{{outcome="{outcome}",le="+Inf"}} {total}')
        lines.append(f'ozon_miner_scrape_duration_seconds_sum{{outcome="{outcome}"}} {h["sum"]:.3f}')
        lines.append(f'ozon_miner_scrape_duration_seconds_count{{outcome="{outcome}"}} {total}')
    return lines

_FETCH_JS = """
async (path) => {
  const url = '/api/entrypoint-api.bx/page/json/v2?url=' + encodeURIComponent(path);
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


def _product_path(product_id: str) -> str:
    """Inner-path карточки для entrypoint-api. JS сам делает encodeURIComponent."""
    return f"/product/{product_id}/"


def _search_path(text: str) -> str:
    """Inner-path поисковой выдачи. text НЕ кодируем здесь — encodeURIComponent в
    _FETCH_JS закодирует весь path целиком (как и у карточки)."""
    return f"/search/?text={text}"


def _seller_path(seg: str) -> str:
    """Inner-path витрины продавца: /seller/<slug>-<id>/. seg — сегмент пути из
    ссылки (slug с числовым id на хвосте). Тот же entrypoint-api, что и у выдачи."""
    return f"/seller/{seg}/"


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
                            c["name"], _first_line(e))
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
    """Залогиненная camoufox-сессия через свой прокси. Сериализует запросы,
    держит человекоподобный интервал (с джиттером), прогревается до healthy с
    проверкой что FAB реально пройден, и умеет ротировать IP."""

    def __init__(self, cfg: dict):
        self.idx = cfg["idx"]
        self.proxy = cfg["proxy"]
        self.cookie = cfg["cookie"]
        self.lock = asyncio.Lock()
        self.healthy = False
        self.egress_ip = ""
        self._last_at = 0.0          # monotonic последнего запроса
        self._last_warm = 0.0        # monotonic последнего успешного прогрева
        self._last_rotate = 0.0      # monotonic последней ротации
        self._warm_fails = 0
        self._next_warm = 0.0        # monotonic — раньше не перепрогревать (backoff)
        self._cam = None
        self._browser = None
        self._page = None

    async def _launch(self):
        """Поднять camoufox + страницу + куки (без прогрева). Общий код для
        первого старта и для пересоздания после смерти драйвера."""
        kw = {"headless": HEADLESS}
        proxy = _parse_proxy(self.proxy)
        if proxy:
            kw["proxy"] = proxy
        # geoip выравнивает локаль/таймзону/гео под exit-IP (рекомендуется при
        # прокси). Если extra не установлен — фолбэк без него.
        try:
            self._cam = AsyncCamoufox(geoip=True, **kw)
            self._browser = await self._cam.__aenter__()
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: geoip недоступен (%s) — без него",
                        self.idx, _first_line(e))
            self._cam = AsyncCamoufox(**kw)
            self._browser = await self._cam.__aenter__()
        self._page = await self._browser.new_page()
        await _add_cookies_safe(self._page.context, _cookie_jar(self.cookie))

    async def start(self):
        await self._launch()
        self._last_rotate = time.monotonic()
        await self.warm()

    async def _relaunch(self) -> bool:
        """Пересоздать camoufox после смерти драйвера/браузера. Без этого дорожка
        вечно крутится на трупе страницы (warm() лишь ре-навигирует мёртвый _page).
        Возвращает True, если новый браузер поднялся."""
        log.warning("дорожка %d: драйвер мёртв — пересоздаю браузер", self.idx)
        await self.close()
        self._cam = self._browser = self._page = None
        try:
            await self._launch()
            return True
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: пересоздание браузера упало: %s",
                      self.idx, _first_line(e))
            return False

    async def warm(self):
        """Навигация на карточку + ожидание, что FAB пройден (тестовый fetch=200).
        Успех → healthy, сброс backoff. Неудача → экспоненциальный backoff."""
        log.info("дорожка %d: прогрев — навигация на %s", self.idx, WARM_URL)
        try:
            await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                  timeout=int(NAV_TIMEOUT_S * 1000))
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: навигация прогрева: %s",
                        self.idx, _first_line(e))
            # Драйвер мёртв → пересоздать браузер и повторить навигацию один раз.
            if _is_dead(e) and await self._relaunch():
                try:
                    await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                          timeout=int(NAV_TIMEOUT_S * 1000))
                except Exception as e2:  # noqa: BLE001
                    log.warning("дорожка %d: навигация после пересоздания: %s",
                                self.idx, _first_line(e2))
        self.egress_ip = await self._egress_ip()
        log.info("дорожка %d: навигация ок (egress=%s), жду прохождения FAB…",
                 self.idx, self.egress_ip or "?")
        deadline = time.time() + WARM_WAIT_S
        warm_path = _product_path(WARM_PRODUCT_ID)
        while time.time() < deadline:
            status, body = await self._inpage_fetch(warm_path)
            if status == 200 and not _looks_blocked(status, body):
                self.healthy = True
                self._warm_fails = 0
                self._next_warm = 0.0
                self._last_warm = time.monotonic()
                log.info("дорожка %d прогрета: egress=%s, FAB пройден",
                         self.idx, self.egress_ip or "?")
                return
            await self._nudge()
            # Не голый wait_for_timeout: на мёртвом драйвере он бросал исключение
            # ВВЕРХ из warm() (до строк healthy=False/backoff ниже) — дорожка
            # застревала healthy=True на трупе. Фолбэк на asyncio.sleep.
            try:
                await self._page.wait_for_timeout(3000)
            except Exception:  # noqa: BLE001
                await asyncio.sleep(3)
        # не прогрелась — backoff, чтобы не долбить FAB (это и жжёт IP)
        self.healthy = False
        self._warm_fails += 1
        backoff = min(MAINT_INTERVAL_S * (2 ** self._warm_fails), WARM_BACKOFF_MAX_S)
        self._next_warm = time.monotonic() + backoff
        log.warning("дорожка %d: прогрев не дал 200 (попыток подряд %d) — backoff %.0fс",
                    self.idx, self._warm_fails, backoff)

    async def rotate(self):
        """Дёрнуть switch-ссылку провайдера (смена exit-IP) и пере-прогреться."""
        if ROTATE_URL:
            try:
                async with aiohttp.ClientSession() as s:
                    async with s.get(ROTATE_URL, timeout=aiohttp.ClientTimeout(total=20)) as r:
                        await r.text()
                log.info("дорожка %d: ротация IP (switch-link дёрнут)", self.idx)
            except Exception as e:  # noqa: BLE001
                log.warning("дорожка %d: ротация не удалась: %s", self.idx, _first_line(e))
            await asyncio.sleep(ROTATE_SETTLE_S)
        self._last_rotate = time.monotonic()
        await self.warm()

    async def fetch_path(self, path: str, label: str):
        """In-page fetch произвольного entrypoint-path (карточка ИЛИ выдача).
        Возвращает (status, body_bytes).

        ЛАТЕНТНОСТЬ: на СТОЙКИЙ FAB (403/тело с fab_) НЕ долбим ретраями — в пределах
        одного вызова это не помогает (доверие восстанавливает только перепрогрев),
        зато растягивает latency (источник хвоста p99=30с) и жжёт IP. Поэтому при FAB
        сразу метим дорожку нездоровой и выходим (перепрогрев сделает maintenance_loop).
        Ретраим ТОЛЬКО транзиентные пустышки (таймаут/0/иной не-200)."""
        async with self.lock:
            t0 = time.monotonic()
            spacing = max(0.1, LANE_MIN_INTERVAL_S * (1.0 + LANE_JITTER * (2 * random.random() - 1)))
            wait = spacing - (t0 - self._last_at)
            if wait > 0:
                await asyncio.sleep(wait)
            waited = time.monotonic() - t0
            status, body = 0, ""
            attempt = 0
            for attempt in range(1, SCRAPE_RETRIES + 1):
                self._last_at = time.monotonic()
                status, body = await self._inpage_fetch(path)
                if status == 200 and not _looks_blocked(status, body):
                    elapsed = time.monotonic() - t0
                    _observe_latency("ok", elapsed)
                    log.info("дорожка %d: %s ок за %.2fс (спейсинг %.2fс, попыток %d)",
                             self.idx, label, elapsed, waited, attempt)
                    return 200, body.encode("utf-8")
                if _looks_blocked(status, body):
                    break  # FAB — ретрай бесполезен, выходим быстро (см. docstring)
                if attempt < SCRAPE_RETRIES:  # транзиент — короткая пауза и ещё попытка
                    await self._nudge()
                    await self._page.wait_for_timeout(RETRY_PAUSE_MS)
            self.healthy = False
            elapsed = time.monotonic() - t0
            _observe_latency("blocked", elapsed)
            log.warning("дорожка %d: FAB/блок на %s (status=%s) за %.2fс попыток %d — нездорова",
                        self.idx, label, status, elapsed, attempt)
            return 403, (body or "").encode("utf-8")

    async def _inpage_fetch(self, path: str):
        try:
            res = await asyncio.wait_for(
                self._page.evaluate(_FETCH_JS, path), timeout=SCRAPE_TIMEOUT_S)
        except Exception as e:  # noqa: BLE001
            log.warning("дорожка %d: fetch упал: %s", self.idx, _first_line(e))
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

    def due_rotate(self, now: float) -> bool:
        return ROTATE_INTERVAL_S > 0 and (now - self._last_rotate) >= ROTATE_INTERVAL_S

    def due_keepalive(self, now: float) -> bool:
        return (self.healthy and WARM_KEEPALIVE_S > 0
                and (now - self._last_warm) >= WARM_KEEPALIVE_S)

    def due_rewarm(self, now: float) -> bool:
        return (not self.healthy) and now >= self._next_warm

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

    def pick_any(self):
        """Любая живая дорожка (для поиска — нет product_id для шардирования)."""
        alive = [l for l in self.lanes if l.healthy]
        return random.choice(alive) if alive else None

    def healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy)

    async def maintenance_loop(self):
        """Раз в MAINT_INTERVAL_S: ротация по расписанию, перепрогрев нездоровых
        (с backoff), keepalive-прогрев живых. Всё под локом дорожки — не мешает
        скрейпу в полёте."""
        while True:
            await asyncio.sleep(MAINT_INTERVAL_S)
            now = time.monotonic()
            for lane in self.lanes:
                if lane.lock.locked():
                    continue
                try:
                    if lane.due_rotate(now):
                        async with lane.lock:
                            log.info("дорожка %d: плановая ротация IP", lane.idx)
                            await lane.rotate()
                    elif lane.due_rewarm(now):
                        async with lane.lock:
                            log.info("дорожка %d нездорова — перепрогрев", lane.idx)
                            await lane.warm()
                    elif lane.due_keepalive(now):
                        async with lane.lock:
                            log.info("дорожка %d: keepalive-прогрев", lane.idx)
                            await lane.warm()
                except Exception as e:  # noqa: BLE001
                    log.error("дорожка %d: обслуживание упало: %s", lane.idx, e)


# ── HTTP ─────────────────────────────────────────────────────────────────────
async def handle_scrape(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    product_id = (request.query.get("id") or "").strip()
    if not product_id.isdigit():
        return web.json_response({"error": "id must be numeric"}, status=400)
    lane = pool.pick(product_id)
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_path(_product_path(product_id), f"product:{product_id}")
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body,
                        content_type="application/json",
                        headers={"X-Ozon-Lane": str(lane.idx)})


async def handle_search(request: web.Request) -> web.Response:
    """GET /search?text=<запрос> → выдача Ozon (виджет searchResultsV2) тем же
    in-page fetch из прогретой дорожки. Возвращает сырой widgetStates, зеркаля
    upstream-статус (403 при FAB). Go-сторона парсит searchResultsV2."""
    pool: Pool = request.app["pool"]
    text = (request.query.get("text") or "").strip()
    if not text:
        return web.json_response({"error": "text required"}, status=400)
    lane = pool.pick_any()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_path(_search_path(text), f"search:{text[:40]}")
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body,
                        content_type="application/json",
                        headers={"X-Ozon-Lane": str(lane.idx)})


async def handle_seller(request: web.Request) -> web.Response:
    """GET /seller?path=<slug-id> → витрина продавца Ozon тем же in-page fetch.
    path — сегмент из ссылки /seller/<slug-id>/. Возвращает сырой widgetStates
    (Go парсит тем же tileGrid-парсером, что и выдачу)."""
    pool: Pool = request.app["pool"]
    seg = (request.query.get("path") or "").strip().strip("/")
    if not seg:
        return web.json_response({"error": "path required"}, status=400)
    lane = pool.pick_any()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_path(_seller_path(seg), f"seller:{seg[:40]}")
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body,
                        content_type="application/json",
                        headers={"X-Ozon-Lane": str(lane.idx)})


async def handle_page(request: web.Request) -> web.Response:
    """GET /page?path=<inner-path> → in-page fetch произвольного entrypoint-path
    (для пагинации: Go передаёт nextPage из предыдущего ответа). path должен
    начинаться с '/'. Возвращает сырой widgetStates."""
    pool: Pool = request.app["pool"]
    path = (request.query.get("path") or "").strip()
    if not path.startswith("/"):
        return web.json_response({"error": "path required (must start with /)"}, status=400)
    lane = pool.pick_any()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_path(path, f"page:{path[:60]}")
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


async def handle_metrics(request: web.Request) -> web.Response:
    """Prometheus-метрики (text exposition) без зависимости prometheus_client —
    формат простой, отдаём строкой. Ключевая для алертов — ozon_miner_healthy_lanes:
    0 = все дорожки мертвы (протух refresh-token/FAB-пропуск → нужен переинжект
    кук). Процесс-даун ловится отдельно через up{job=~"tryberrybot-.+"}."""
    pool: Pool = request.app["pool"]
    lines = [
        "# HELP ozon_miner_healthy_lanes Число прогретых (healthy) дорожек camoufox",
        "# TYPE ozon_miner_healthy_lanes gauge",
        f"ozon_miner_healthy_lanes {pool.healthy_count()}",
        "# HELP ozon_miner_total_lanes Всего сконфигурённых дорожек",
        "# TYPE ozon_miner_total_lanes gauge",
        f"ozon_miner_total_lanes {len(pool.lanes)}",
        "# HELP ozon_miner_lane_healthy Здоровье конкретной дорожки (1=healthy, 0=нет)",
        "# TYPE ozon_miner_lane_healthy gauge",
    ]
    for l in pool.lanes:
        lines.append(f'ozon_miner_lane_healthy{{lane="{l.idx}"}} {1 if l.healthy else 0}')
    lines.extend(_latency_metric_lines())
    return web.Response(text="\n".join(lines) + "\n", content_type="text/plain")


async def main():
    configs = _load_lane_configs()
    if not configs:
        raise SystemExit("нет ни одной сконфигурённой дорожки: задай OZON_COOKIE "
                         "(дорожка 0) или OZON_LANE_<i>_COOKIE")
    log.info("старт ozon-miner: port=%d дорожек=%d (POOL_SIZE=%d) движок=camoufox "
             "ротация=%s keepalive=%.0fмин",
             PORT, len(configs), POOL_SIZE,
             f"{ROTATE_INTERVAL_S/60:.0f}мин" if ROTATE_INTERVAL_S > 0 else "выкл",
             WARM_KEEPALIVE_S / 60)

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
    app.router.add_get("/search", handle_search)
    app.router.add_get("/seller", handle_seller)
    app.router.add_get("/page", handle_page)
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
