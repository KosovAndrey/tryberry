#!/usr/bin/env python3
"""
server.py — ali-miner: БРАУЗЕР-КАК-ТРАНСПОРТ для поисковой выдачи aliexpress.ru.

Долгоживущий HTTP-сервис с пулом прогретых дорожек (lane). Дорожка = headful
Chromium (patchright, тот же движок, что проходит wbaas в wb-search-miner) в Xvfb.

Зачем: голый direct-POST к `/aer-webapi/v1/search` с датацентр-IP X5SEC режет
(punish/слайдер), хотя карточный `/aer-jsonapi/.../productData` с той же
aer-cookie проходит — выдачу X5SEC проверяет строже. Браузер проходит челлендж
живой сессией, поэтому горячий путь выдачи уводим сюда — как у WB (wb-search-miner)
и Ozon (ozon-miner).

Дорожка прогревается навигацией на страницу выдачи нейтрального запроса
(https://aliexpress.ru/wholesale?SearchText=...). Прогрев успешен, когда
СОБСТВЕННЫЙ search-XHR страницы вернул 200 и в теле есть товары (нет
punish-маркеров) — значит X5SEC пройден, cookie сессии доверенные.

Go-скрейпер (AliexpressSearchScraper, фолбэк на X5SEC-блок) зовёт
GET /search?text=<q>&page=<n> → дорожка навигирует на страницу запроса и
ПЕРЕХВАТЫВАЕТ нативный ответ /aer-webapi/v1/search фронта (реплей руками не
делаем — у WB такой in-page fetch антибот резал, у Ali фронт тоже кладёт в
запрос волатильные bx-заголовки/подпись). Отдаёт СЫРОЙ JSON той же формы, что
и direct (parseAliexpressSearch общий), зеркаля upstream-статус.

Живучесть: джиттер интервала, backoff на стойкой стене, периодический re-warm,
пересоздание браузера после смерти драйвера и после N неудачных прогревов подряд
(свежий фингерпринт). Прокси НЕ нужен (карточная дорожка доказала, что X5SEC
пускает датацентр-IP при живой сессии), но опционально поддержан через
ALI_SEARCH_PROXY_URL / ALI_LANE_<i>_PROXY — на случай, если X5SEC ужесточится
(тогда дешёвый резидентский RU по ГБ только сюда).
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
PORT = int(os.getenv("ALI_MINER_PORT", "8082"))
POOL_SIZE = int(os.getenv("ALI_SEARCH_POOL_SIZE", "1"))

WARM_QUERY = os.getenv("ALI_WARM_QUERY", "телефон")
SEARCH_PAGE_URL = os.getenv(
    "ALI_SEARCH_PAGE_URL",
    "https://aliexpress.ru/wholesale?SearchText={query}",
)
# Маркер search-XHR в URL ответов (нативный запрос фронта). Должен совпадать с
# aliSearchAPI в Go (internal/scraper/aliexpress_search.go).
SEARCH_MARKER = os.getenv("ALI_SEARCH_MARKER", "/aer-webapi/v1/search")

HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
FETCH_TIMEOUT_S = float(os.getenv("ALI_FETCH_TIMEOUT_SECONDS", "20"))
NAV_TIMEOUT_S = float(os.getenv("ALI_NAV_TIMEOUT_SECONDS", "60"))
WARM_WAIT_S = float(os.getenv("ALI_WARM_WAIT_SECONDS", "150"))
WARM_RELOADS = int(os.getenv("ALI_WARM_RELOADS", "2"))

# Человекоподобный интервал между запросами одной дорожки + джиттер.
LANE_MIN_INTERVAL_S = float(os.getenv("ALI_LANE_MIN_INTERVAL_MS", "1500")) / 1000.0
LANE_JITTER = float(os.getenv("ALI_LANE_JITTER", "0.4"))

# Плановое пересоздание браузера по ВОЗРАСТУ. Память дорожки растёт по аптайму,
# а не по числу запросов: 20-08-2026 chrome этого сайдкара дорос до 6.7 ГиБ
# (штатно ~0.8) за 7 дней и увёл ВЕСЬ хост в OOM — ядро прибило его, а вместе с
# ним на минуту легли DNS Docker и половина мониторинга. mem_limit (2g) сделал
# такой отказ локальным, но рост не лечит: без recycle дорожку будет тихо
# убивать раз в пару суток. 0 = выключить.
LANE_MAX_AGE_S = float(os.getenv("ALI_LANE_MAX_AGE_SECONDS", "21600"))
# Разброс порога по дорожкам, чтобы пул не пересоздавался весь разом и поиск
# не проваливался в 502 на время общего прогрева.
LANE_MAX_AGE_JITTER = float(os.getenv("ALI_LANE_MAX_AGE_JITTER", "0.2"))

MAINT_INTERVAL_S = float(os.getenv("ALI_HEALTH_INTERVAL_SECONDS", "30"))
# Лок дорожки держат дольше этого — считаем её залипшей. Порог с запасом над
# самым долгим штатным запросом (навигация + ожидание ответа search).
LANE_STUCK_S = float(os.getenv("ALI_LANE_STUCK_SECONDS", "180"))
# Дедлайн на операцию обслуживания: без него `await warm()` на подвисшем
# драйвере морозит ремонт всего пула.
MAINT_OP_TIMEOUT_S = float(os.getenv("ALI_MAINT_OP_TIMEOUT_SECONDS", "240"))
# Дедлайн на чтение тела ответа: единственный await в fetch_search, у которого
# своего тайм-аута нет. Именно на нём вставал wb-search-miner (инцидент 13-08).
BODY_TIMEOUT_S = float(os.getenv("ALI_BODY_TIMEOUT_SECONDS", "30"))
# Запас поверх собственного таймера Playwright у навигации: своя граница нужна
# на случай мёртвого CDP-соединения, когда таймер драйвера не срабатывает.
# Вынесен в параметр, иначе нижнюю границу ожидания не проверить тестом.
NAV_HARD_SLACK_S = float(os.getenv("ALI_NAV_HARD_SLACK_SECONDS", "10"))
# Дедлайн на необязательные действия мыши.
NUDGE_TIMEOUT_S = float(os.getenv("ALI_NUDGE_TIMEOUT_SECONDS", "15"))
WARM_KEEPALIVE_S = float(os.getenv("ALI_WARM_KEEPALIVE_MINUTES", "30")) * 60.0
WARM_BACKOFF_MAX_S = float(os.getenv("ALI_WARM_BACKOFF_MAX_SECONDS", "600"))
# После скольких ПОДРЯД неудачных прогревов пересоздать браузер (свежий контекст/
# фингерпринт) вместо долбёжки того же walled-контекста. 0 — не пересоздавать.
WARM_RELAUNCH_AFTER = int(os.getenv("ALI_WARM_RELAUNCH_AFTER", "3"))

LOCALE = os.getenv("ALI_MINER_LOCALE", "ru-RU")
TIMEZONE = os.getenv("ALI_MINER_TIMEZONE", "Europe/Moscow")
ALI_USER_AGENT = os.getenv("ALI_USER_AGENT", "").strip()
BLOCK_RESOURCES = os.getenv("MINER_BLOCK_RESOURCES", "true").lower() in ("1", "true", "yes")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("ali-miner")

# Маркеры X5SEC-стены в теле ответа/страницы (те же, что isAliBlocked в Go).
BLOCK_MARKERS = (b"x5secdata", b"_____tmd_____", b"punish")
# Признак живой выдачи в теле search-ответа (тот же, что aliHasProducts в Go).
PRODUCTS_MARKER = b'"snippetContainer"'

# Признаки смерти драйвера/браузера — дорожку надо пересоздать, а не
# ре-навигировать труп.
_DEAD_RE = re.compile(
    r"Connection closed|pipe closed|Target (page|frame|browser).*closed|"
    r"Browser.*closed|has been closed|Target closed",
    re.IGNORECASE)


# Момент последнего УСПЕХА пула (успешный запрос или прогрев). Гонится в метрику
# ali_miner_last_success_age_seconds: healthy_lanes врёт при залипании (дорожка
# числится живой, но не работает), а этот возраст — нет.
_last_success_at = time.monotonic()


def _mark_success():
    global _last_success_at
    _last_success_at = time.monotonic()


async def _quiet_call(coro, timeout: float, what: str, idx: int):
    """Вызов драйвера с дедлайном: тайм-аут/ошибка → False, без исключения
    наверх. Для операций, где важно одно — не зависнуть навсегда."""
    try:
        await asyncio.wait_for(coro, timeout=timeout)
        return True
    except asyncio.TimeoutError:
        log.warning("дорожка %d: %s не уложилось в %.0fс", idx, what, timeout)
    except Exception as e:  # noqa: BLE001
        log.warning("дорожка %d: %s упало: %s", idx, what, _first_line(e))
    return False


def _is_dead(exc) -> bool:
    return bool(_DEAD_RE.search(str(exc)))


def _first_line(exc) -> str:
    return (str(exc).splitlines() or [""])[0]


def _blocked(body: bytes) -> bool:
    return any(m in body for m in BLOCK_MARKERS)


def _search_page_url(text: str, page: int) -> str:
    """URL страницы выдачи для навигации (фронт сам дёрнет /aer-webapi/v1/search).
    page — штатный query-параметр выдачи aliexpress.ru."""
    url = SEARCH_PAGE_URL.format(query=quote(text))
    if page and page > 1:
        url += "&page=" + str(page)
    return url


def _resp_matches(resp, page: int) -> bool:
    """Наш ли это search-XHR: маркер в URL + (для page>1) совпадение страницы в
    POST-теле — фронт на одной навигации может дёрнуть search не один раз."""
    if SEARCH_MARKER not in resp.url:
        return False
    if page <= 1:
        return True
    try:
        pd = resp.request.post_data or ""
    except Exception:  # noqa: BLE001
        pd = ""
    return f'"page":{page}' in pd.replace(" ", "")


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


# ── Дорожка ───────────────────────────────────────────────────────────────────
class Lane:
    def __init__(self, idx: int, proxy: str):
        self.idx = idx
        self.proxy = proxy
        self.lock = asyncio.Lock()
        self.healthy = False
        self._lock_since = 0.0      # когда лок захвачен (0 = свободен)
        self._servicing = False     # обслуживание уже запущено задачей
        self._needs_relaunch = False
        self._last_at = 0.0
        self._last_warm = 0.0
        self._warm_fails = 0            # подряд неудач (для backoff)
        self._fails_since_relaunch = 0  # подряд неудач с последнего relaunch
        self._next_warm = 0.0
        self._launched_at = 0.0  # когда браузер создан (для recycle по возрасту)
        self._age_limit = 0.0    # персональный порог возраста с джиттером
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
        if ALI_USER_AGENT:
            ctx_kw["user_agent"] = ALI_USER_AGENT
        self._ctx = await self._browser.new_context(**ctx_kw)
        self._page = await self._ctx.new_page()
        if BLOCK_RESOURCES:
            async def _route(route):
                # Блокируем ТОЛЬКО image/media (трафик). Шрифты НЕ трогаем — их
                # отсутствие меняет canvas/font-fingerprint, антибот это палит
                # (урок wbaas). Обязательно await — fire-and-forget рушил бы
                # загрузку/челлендж.
                try:
                    if route.request.resource_type in ("image", "media"):
                        await route.abort()
                    else:
                        await route.continue_()
                except Exception:  # noqa: BLE001
                    pass
            await self._page.route("**/*", _route)
        self._launched_at = time.monotonic()
        self._age_limit = LANE_MAX_AGE_S * (1.0 + random.uniform(0.0, LANE_MAX_AGE_JITTER))

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
        # Движение мыши + колесо: антибот смотрит на человекоподобные события
        # указателя.
        #
        # С дедлайном: нудж необязателен, а на подвисшем драйвере операция мыши
        # не возвращается — и держит лок дорожки вместе с собой.
        await _quiet_call(
            self._page.mouse.move(random.randint(80, 1280), random.randint(80, 700),
                                  steps=random.randint(4, 9)),
            NUDGE_TIMEOUT_S, "нудж (мышь)", self.idx)
        await _quiet_call(self._page.mouse.wheel(0, random.randint(200, 1100)),
                          NUDGE_TIMEOUT_S, "нудж (колесо)", self.idx)

    async def _human_browse(self):
        # Живой browse на выдаче: прокрутки с паузами + движения указателя —
        # профилактика стены на холодную (уже выданную punish-страницу не снимает).
        for _ in range(random.randint(2, 4)):
            await self._nudge()
            try:
                await self._page.mouse.wheel(0, random.randint(400, 1500))
            except Exception:  # noqa: BLE001
                pass
            try:
                await self._page.wait_for_timeout(random.randint(500, 1600))
            except Exception:  # noqa: BLE001
                await asyncio.sleep(random.uniform(0.5, 1.6))

    async def warm(self):
        """Прогрев: навигация на выдачу нейтрального запроса, СЛУШАЕМ ответы —
        ждём search-XHR самой страницы со статусом 200, телом без punish-маркеров
        и с товарами (= X5SEC пройден). Успех → healthy; неудача → backoff."""
        # Залипание: N неудач подряд → текущий контекст, скорее всего, walled
        # (X5SEC отдаёт punish на самой навигации). Пересоздаём браузер со свежим
        # фингерпринтом ПЕРЕД прогревом.
        if WARM_RELAUNCH_AFTER > 0 and self._fails_since_relaunch >= WARM_RELAUNCH_AFTER:
            log.warning("дорожка %d: %d неудач подряд — пересоздаю браузер перед прогревом",
                        self.idx, self._fails_since_relaunch)
            if await self._relaunch():
                self._fails_since_relaunch = 0

        url = _search_page_url(WARM_QUERY, 1)
        seen = {"ok": False, "statuses": [], "all": []}

        async def on_resp(resp):
            try:
                seen["all"].append((resp.request.resource_type, resp.status, resp.url))
                if SEARCH_MARKER in resp.url:
                    seen["statuses"].append(resp.status)
                    if resp.status == 200:
                        try:
                            body = await resp.body()
                        except Exception:  # noqa: BLE001
                            body = b""
                        if not _blocked(body) and PRODUCTS_MARKER in body:
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
                    await self._human_browse()
                if seen["ok"]:
                    break
                log.info("дорожка %d: попытка %d, живой выдачи нет (search: %s)",
                         self.idx, attempt, seen["statuses"][-6:] or "—")
        finally:
            try:
                self._page.remove_listener("response", on_resp)
            except Exception:  # noqa: BLE001
                pass

        if seen["ok"]:
            self.healthy = True
            _mark_success()
            self._warm_fails = 0
            self._fails_since_relaunch = 0
            self._next_warm = 0.0
            self._last_warm = time.monotonic()
            log.info("дорожка %d прогрета: search 200 с товарами (X5SEC пройден)", self.idx)
            return

        self.healthy = False
        self._warm_fails += 1
        self._fails_since_relaunch += 1
        backoff = min(MAINT_INTERVAL_S * (2 ** self._warm_fails), WARM_BACKOFF_MAX_S)
        self._next_warm = time.monotonic() + backoff
        log.warning("дорожка %d: прогрев без живой выдачи (подряд %d, search: %s) — backoff %.0fс",
                    self.idx, self._warm_fails, seen["statuses"][-6:] or "—", backoff)
        await self._dump_diag(seen)

    async def _dump_diag(self, seen: dict):
        """Диагностика провала прогрева: заголовок/тело страницы + гистограмма
        хостов ответов + ключевые xhr/document — понять, стена это, пусто или
        search ушёл на другой URL."""
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

    async def fetch_search(self, text: str, page: int):
        """Навигируем прогретый браузер на страницу запроса и ПЕРЕХВАТЫВАЕМ ответ
        /aer-webapi/v1/search, который фронт делает сам (нативно, со всеми нужными
        bx-заголовками/подписью). Возвращает (status, body_bytes)."""
        nav_url = _search_page_url(text, page)
        async with self.lock:
            self._lock_since = time.monotonic()
            t0 = time.monotonic()
            spacing = max(0.1, LANE_MIN_INTERVAL_S * (1.0 + LANE_JITTER * (2 * random.random() - 1)))
            wait = spacing - (t0 - self._last_at)
            if wait > 0:
                await asyncio.sleep(wait)
            self._last_at = time.monotonic()

            loop = asyncio.get_event_loop()
            fut: asyncio.Future = loop.create_future()

            def on_resp(resp):
                try:
                    if _resp_matches(resp, page) and not fut.done():
                        fut.set_result(resp)
                except Exception:  # noqa: BLE001
                    pass

            self._page.on("response", on_resp)
            try:
                try:
                    # Дедлайн СВЕРХУ, поверх собственного таймера Playwright:
                    # в инциденте ozon-miner висли именно драйверные вызовы, у
                    # которых свой таймер не срабатывал (мёртвое CDP-соединение).
                    await asyncio.wait_for(
                        self._page.goto(nav_url, wait_until="commit",
                                        timeout=int(NAV_TIMEOUT_S * 1000)),
                        timeout=NAV_TIMEOUT_S + NAV_HARD_SLACK_S)
                except asyncio.TimeoutError:
                    self.healthy = False
                    self._needs_relaunch = True
                    log.warning("дорожка %d: навигация не вернулась за %.0fс — драйвер завис",
                                self.idx, NAV_TIMEOUT_S + NAV_HARD_SLACK_S)
                    return 504, b""
                except Exception as e:  # noqa: BLE001
                    # search часто прилетает ещё до полной загрузки — навигационную
                    # ошибку не считаем фаталом, ждём ответ ниже.
                    if _is_dead(e):
                        self.healthy = False
                        return 502, b""
                await self._nudge()
                try:
                    resp = await asyncio.wait_for(fut, timeout=FETCH_TIMEOUT_S)
                except asyncio.TimeoutError:
                    self.healthy = False
                    log.warning("дорожка %d: search не прилетел на %r p%d — нездорова",
                                self.idx, text[:40], page)
                    return 403, b""
                status = resp.status
                # С дедлайном: единственный await без своего тайм-аута. Тело
                # может не дойти никогда, а корутина держит лок дорожки — и
                # тогда пул стоит при зелёном healthy (так вставал wb-search-miner).
                try:
                    body = await asyncio.wait_for(resp.body(), timeout=BODY_TIMEOUT_S)
                except asyncio.TimeoutError:
                    self.healthy = False
                    self._needs_relaunch = True
                    log.warning("дорожка %d: тело search не дошло за %.0fс — нездорова",
                                self.idx, BODY_TIMEOUT_S)
                    return 504, b""
                except Exception as e:  # noqa: BLE001
                    log.warning("дорожка %d: чтение тела search упало: %s", self.idx, _first_line(e))
                    return 502, b""
                if status == 200 and not _blocked(body):
                    _mark_success()
                    log.info("дорожка %d: search %r p%d ок (%d байт)",
                             self.idx, text[:40], page, len(body))
                    return 200, body
                self.healthy = False
                log.warning("дорожка %d: search status=%s blocked=%s на %r p%d — нездорова",
                            self.idx, status, _blocked(body), text[:40], page)
                return status, body
            finally:
                self._lock_since = 0.0
                try:
                    self._page.remove_listener("response", on_resp)
                except Exception:  # noqa: BLE001
                    pass

    def due_keepalive(self, now: float) -> bool:
        return self.healthy and WARM_KEEPALIVE_S > 0 and (now - self._last_warm) >= WARM_KEEPALIVE_S

    def due_rewarm(self, now: float) -> bool:
        return (not self.healthy) and now >= self._next_warm

    def age(self, now: float) -> float:
        """Сколько секунд живёт текущий браузер (0 = ещё не создан)."""
        return (now - self._launched_at) if self._launched_at else 0.0

    def due_recycle(self, now: float) -> bool:
        """Пора планово пересоздать браузер: он зажился и течёт по памяти.
        Только для здоровой дорожки — нездоровую и так чинит due_rewarm."""
        return (LANE_MAX_AGE_S > 0 and self.healthy
                and self._launched_at > 0 and self._age_limit > 0
                and self.age(now) >= self._age_limit)

    def stuck_for(self, now: float) -> float:
        """Сколько секунд лок дорожки держат сверх LANE_STUCK_S (0 = не залипла)."""
        if not self.lock.locked() or not self._lock_since:
            return 0.0
        held = now - self._lock_since
        return held if held > LANE_STUCK_S else 0.0

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

    def stuck_lanes(self, now: float) -> int:
        return sum(1 for l in self.lanes if l.stuck_for(now) > 0)

    async def maintenance_loop(self):
        """Раз в MAINT_INTERVAL_S: вотчдог залипших дорожек и прогрев по
        расписанию.

        Обслуживание КАЖДОЙ дорожки — отдельная задача с дедлайном. Раньше цикл
        шёл последовательно и под локом: подвисший `await warm()` морозил ремонт
        всего пула, а залипшую дорожку никто не снимал с healthy — она числилась
        живой и молча съедала все запросы (отказ ozon-miner 29-07 и
        wb-search-miner 13-08)."""
        while True:
            await asyncio.sleep(MAINT_INTERVAL_S)
            now = time.monotonic()
            for lane in self.lanes:
                held = lane.stuck_for(now)
                if held > 0:
                    if lane.healthy:
                        log.error("дорожка %d: лок занят %.0fс — залипла, снимаю healthy",
                                  lane.idx, held)
                    lane.healthy = False
                    lane._needs_relaunch = True
                    continue
                if lane.lock.locked() or lane._servicing:
                    continue
                if lane.due_recycle(now):
                    # Снимаем healthy ЧЕСТНО: браузер сейчас исчезнет, и отдавать
                    # на него запросы нельзя. Дальше отработает штатный путь
                    # _service_lane: _needs_relaunch → _relaunch() → due_rewarm →
                    # warm(). _next_warm обнуляем, чтобы прогрев пошёл сразу.
                    log.info("дорожка %d: плановый recycle — браузер живёт %.0fч",
                             lane.idx, lane.age(now) / 3600.0)
                    lane.healthy = False
                    lane._needs_relaunch = True
                    lane._next_warm = 0.0
                elif not (lane.due_rewarm(now) or lane.due_keepalive(now)):
                    continue
                # Флаг ставим ЗДЕСЬ, а не в задаче: между ensure_future и первой
                # строкой задачи цикл успел бы завести вторую такую же.
                lane._servicing = True
                asyncio.ensure_future(self._service_lane(lane))

    async def _service_lane(self, lane):
        """Обслужить одну дорожку под её локом, с дедлайном на операцию."""
        try:
            async with lane.lock:
                lane._lock_since = time.monotonic()
                if lane._needs_relaunch:
                    lane._needs_relaunch = False
                    await _quiet_call(lane._relaunch(), MAINT_OP_TIMEOUT_S,
                                      "пересоздание", lane.idx)
                now = time.monotonic()
                if lane.due_rewarm(now):
                    what = "нездорова — перепрогрев"
                elif lane.due_keepalive(now):
                    what = "keepalive-прогрев"
                else:
                    return
                log.info("дорожка %d: %s", lane.idx, what)
                if not await _quiet_call(lane.warm(), MAINT_OP_TIMEOUT_S,
                                         "обслуживание", lane.idx):
                    lane._needs_relaunch = True
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: обслуживание упало: %s", lane.idx, e)
        finally:
            lane._lock_since = 0.0
            lane._servicing = False


# ── HTTP ────────────────────────────────────────────────────────────────────────
async def handle_search(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    text = (request.query.get("text") or request.query.get("query") or "").strip()
    if not text:
        return web.json_response({"error": "text required"}, status=400)
    try:
        page = max(1, int(request.query.get("page") or "1"))
    except ValueError:
        page = 1
    lane = pool.pick()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_search(text, page)
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body, content_type="application/json",
                        headers={"X-Ali-Lane": str(lane.idx)})


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
        "# HELP ali_miner_healthy_lanes Число прогретых дорожек",
        "# TYPE ali_miner_healthy_lanes gauge",
        f"ali_miner_healthy_lanes {pool.healthy_count()}",
        "# HELP ali_miner_total_lanes Всего дорожек",
        "# TYPE ali_miner_total_lanes gauge",
        f"ali_miner_total_lanes {len(pool.lanes)}",
        "# HELP ali_miner_lane_healthy Здоровье дорожки (1/0)",
        "# TYPE ali_miner_lane_healthy gauge",
    ]
    for l in pool.lanes:
        lines.append(f'ali_miner_lane_healthy{{lane="{l.idx}"}} {1 if l.healthy else 0}')
    now = time.monotonic()
    lines += [
        # Главная метрика живости: healthy_lanes врёт при залипании драйвера
        # (дорожка числится живой, но не отвечает), а возраст успеха — нет.
        "# HELP ali_miner_last_success_age_seconds Секунд с последнего успешного запроса или прогрева",
        "# TYPE ali_miner_last_success_age_seconds gauge",
        f"ali_miner_last_success_age_seconds {now - _last_success_at:.0f}",
        # Возраст браузера: растущая память коррелирует именно с ним.
        # Плато на LANE_MAX_AGE_S = recycle работает; безостановочный рост = нет.
        "# HELP ali_miner_lane_age_seconds Секунд с создания браузера дорожки",
        "# TYPE ali_miner_lane_age_seconds gauge",
    ] + [
        f'ali_miner_lane_age_seconds{{lane="{l.idx}"}} {l.age(now):.0f}'
        for l in pool.lanes
    ] + [
        "# HELP ali_miner_stuck_lanes Дорожек с локом, занятым дольше порога",
        "# TYPE ali_miner_stuck_lanes gauge",
        f"ali_miner_stuck_lanes {pool.stuck_lanes(now)}",
    ]
    return web.Response(text="\n".join(lines) + "\n", content_type="text/plain")


def _lane_proxies():
    out = []
    for i in range(POOL_SIZE):
        p = os.getenv(f"ALI_LANE_{i}_PROXY", "").strip()
        if i == 0:
            p = p or os.getenv("ALI_SEARCH_PROXY_URL", "").strip()
        out.append(p)
    return out


async def main():
    proxies = _lane_proxies()
    log.info("старт ali-miner: port=%d дорожек=%d warm=%r headless=%s proxy=%s",
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
