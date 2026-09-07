#!/usr/bin/env python3
"""
server.py — ozon-miner, фаза 2: БРАУЗЕР-КАК-ТРАНСПОРТ с ПУЛОМ ДОРОЖЕК (camoufox).

Долгоживущий HTTP-сервис. Держит пул из N «дорожек» (lane). Дорожка = отдельный
браузер **camoufox** (анти-детект Firefox) через свой прокси. camoufox проходит
антибот FAB (пробил его там, где голый Chromium палился) БЕЗ логина, поэтому
дорожки по умолчанию АНОНИМНЫЕ (без cookie). Аккаунт-cookie нужен ТОЛЬКО под
товары 18+ (возрастной гейт) — такая дорожка помечается authed и резервируется
под 18+, обычный поток идёт через анонимные (аккаунт бережём от бана).
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

Масштабирование = добавить дорожек: OZON_POOL_SIZE + OZON_LANE_<i>_PROXY (cookie
задавать НЕ обязательно — без него дорожка анонимная). Дорожка 0 фолбэчит на
OZON_PROXY_URL / OZON_COOKIE. Route /scrape?id=X&authed=1 → authed-дорожка (18+).
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

# Плановое пересоздание браузера по ВОЗРАСТУ. Раньше здесь его сознательно НЕ
# было: считалось, что пере-прохождение FAB дороже пользы, а память перекрывает
# mem_limit. Инцидент 02-09-2026 это опроверг — цена не в памяти. Контейнер
# простоял 5 суток, и ЗАЖИВШИЙСЯ браузер перестаёт проходить FAB: пять из шести
# дорожек не могли прогреться («прогрев не дал 200»), за неделю каждая анонимная
# была здорова лишь 16% времени, весь поток сериализовался на выжившей
# authed-дорожке и p95 скрейпа уехал на 26-29с. Пересоздание вернуло 6/6 разом.
# То есть FAB мы всё равно теряем, только не осознанно раз в N часов, а исподволь
# и без предупреждения. 0 = выключить.
LANE_MAX_AGE_S = float(os.getenv("OZON_LANE_MAX_AGE_SECONDS", "21600"))
# Разброс порога по дорожкам, чтобы пул не пересоздавался разом и запросы не
# упирались в 502 на время общего прогрева.
LANE_MAX_AGE_JITTER = float(os.getenv("OZON_LANE_MAX_AGE_JITTER", "0.2"))
# Потолок backoff при неудачных прогревах (не долбить FAB).
WARM_BACKOFF_MAX_S = float(os.getenv("OZON_WARM_BACKOFF_MAX_SECONDS", "600"))

# Ротация IP по switch-ссылке провайдера. Пусто → ротация выкл.
ROTATE_URL = os.getenv("OZON_PROXY_ROTATE_URL", "").strip()
# Как часто ротировать (минуты). 0 → выкл. Частую ставить можно, но каждая
# ротация = пере-прогрев (заново пройти FAB на новом IP, дорожка ~минуту занята).
ROTATE_INTERVAL_S = float(os.getenv("OZON_ROTATE_INTERVAL_MINUTES", "0")) * 60.0
# Пауза после дёрганья switch-ссылки, чтобы прокси успел сменить exit-IP.
ROTATE_SETTLE_S = float(os.getenv("OZON_ROTATE_SETTLE_SECONDS", "6"))

# ── Потолки на ЛЮБОЙ вызов драйвера (защита от дедлока 29-07) ────────────────
# Инцидент: подвисший драйвер намертво держал лок дорожки (пауза между ретраями
# шла ЧЕРЕЗ браузер — page.wait_for_timeout), обслуживающий цикл пропускал
# залоченные дорожки, и пул 11 часов стоял с healthy=4 при нуле работы. Правило
# теперь простое: ни один await к драйверу не бывает без дедлайна.
# Потолок ОДНОЙ операции под локом (fetch + ретраи + спейсинг).
LANE_OP_TIMEOUT_S = float(os.getenv(
    "OZON_LANE_OP_TIMEOUT_SECONDS",
    str(SCRAPE_TIMEOUT_S * SCRAPE_RETRIES + RETRY_PAUSE_MS / 1000.0 + 15)))
# Потолок операции обслуживания (прогрев/ротация — там навигация + ожидание FAB).
MAINT_OP_TIMEOUT_S = float(os.getenv("OZON_MAINT_OP_TIMEOUT_SECONDS",
                                     str(NAV_TIMEOUT_S + WARM_WAIT_S + 60)))
# Потолок «мелких» вызовов драйвера (нудж, egress-IP, закрытие браузера).
DRIVER_CALL_TIMEOUT_S = float(os.getenv("OZON_DRIVER_CALL_TIMEOUT_SECONDS", "20"))
# Лок держат дольше этого → дорожка залипла: снимаем healthy (пул её обойдёт) и
# помечаем на пересоздание браузера. Страховка на случай, если wait_for выше не
# смог отменить зависший await.
LANE_STUCK_S = float(os.getenv("OZON_LANE_STUCK_SECONDS",
                               str(max(LANE_OP_TIMEOUT_S, MAINT_OP_TIMEOUT_S) + 60)))

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
    r"Browser.*closed|has been closed|Target closed|Navigation failed because browser|"
    # «Page crashed»/«Target crashed» — рендерер убит (обычно OOM в cgroup).
    # См. разбор в ali-miner/server.py: без этого дорожка лечится минутами.
    r"crashed",
    re.IGNORECASE)


def _is_dead(exc) -> bool:
    return bool(_DEAD_RE.search(str(exc)))


def _first_line(exc) -> str:
    """Первая строка текста исключения. str(exc).splitlines()[0] падал с
    IndexError, когда у исключения пустой текст (splitlines() → []) — и ронял
    обработчик ошибки вместо логирования."""
    return (str(exc).splitlines() or [""])[0]


# Момент последнего УСПЕХА пула (успешный fetch или прогрев). Гонится в метрику
# ozon_miner_last_success_age_seconds: healthy_lanes врёт при залипании (дорожка
# числится живой, но не работает), а этот возраст — нет.
_last_success_at = time.monotonic()


def _mark_success():
    global _last_success_at
    _last_success_at = time.monotonic()


# Сколько раз массовый поток (товары/выдача/витрина) не нашёл ЖИВОЙ АНОНИМНОЙ
# дорожки. Раньше в этот момент был тихий фолбэк на authed — 07-09-2026 он десять
# часов гнал ~250 запросов/час под аккаунтом, пока FAB держал анонимные дорожки
# в отказе. Теперь отдаём 502 (Go зажигает брейкер, площадка деградирует ЯВНО),
# а счётчик показывает, что именно это и происходит.
_anon_starved_total = 0


def _mark_anon_starved():
    global _anon_starved_total
    _anon_starved_total += 1


async def _quiet_call(coro, timeout: float, what: str, idx: int):
    """Вызов драйвера с дедлайном: тайм-аут/ошибка → False, без исключения наверх.
    Для необязательных операций (нудж, egress-IP, закрытие браузера), где важно
    только одно — не зависнуть навсегда."""
    try:
        await asyncio.wait_for(coro, timeout=timeout)
        return True
    except asyncio.TimeoutError:
        log.warning("дорожка %d: %s не уложился в %.0fс — бросаю", idx, what, timeout)
    except Exception as e:  # noqa: BLE001
        log.debug("дорожка %d: %s: %s", idx, what, _first_line(e))
    return False

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
    """Собрать дорожки из env. Дорожка БЕЗ cookie — валидна и штатна (анонимная):
    camoufox проходит FAB без логина, аккаунт нужен ТОЛЬКО для товаров 18+
    (возрастной гейт). Дорожка С cookie (залогинена, 18+ подтверждён) резервируется
    под 18+; обычный поток идёт через анонимные (аккаунт бережём от бана)."""
    configs = []
    for i in range(POOL_SIZE):
        proxy = os.getenv(f"OZON_LANE_{i}_PROXY", "").strip()
        cookie = os.getenv(f"OZON_LANE_{i}_COOKIE", "").strip()
        if i == 0:
            proxy = proxy or os.getenv("OZON_PROXY_URL", "").strip()
            cookie = cookie or os.getenv("OZON_COOKIE", "").strip()
        configs.append({"idx": i, "proxy": proxy, "cookie": cookie})
    return configs


# ── Память своего контейнера (cgroup v2 виден изнутри) ───────────────────────
# Метрик уровня контейнера в Prometheus нет: cAdvisor не подключён, и вопрос
# «расход вышел на плато или ползёт» приходилось решать самодельными скриптами
# по ssh (02-09-2026 так и ловили рост ali). Сайдкар видит свой cgroup сам —
# дешевле отдать два числа в /metrics и получить историю с алертом.
_CGROUP_CURRENT = "/sys/fs/cgroup/memory.current"
_CGROUP_MAX = "/sys/fs/cgroup/memory.max"
_CGROUP_STAT = "/sys/fs/cgroup/memory.stat"


def _cgroup_bytes(path: str):
    """Число из файла cgroup или None (нет cgroup v2 / "max" / нет доступа)."""
    try:
        with open(path) as f:
            return int(f.read().strip())
    except Exception:  # noqa: BLE001
        return None


# ── Дорожка (lane) ───────────────────────────────────────────────────────────
class Lane:
    """Залогиненная camoufox-сессия через свой прокси. Сериализует запросы,
    держит человекоподобный интервал (с джиттером), прогревается до healthy с
    проверкой что FAB реально пройден, и умеет ротировать IP."""

    def __init__(self, cfg: dict):
        self.idx = cfg["idx"]
        self.proxy = cfg["proxy"]
        self.cookie = cfg["cookie"]
        # authed = залогиненная сессия (есть access-token) → умеет 18+. Анонимная
        # дорожка (authed=False) обычные товары тянет, но на 18+ упрётся в гейт.
        self.authed = "__Secure-access-token" in self.cookie
        self.lock = asyncio.Lock()
        self.healthy = False
        self.egress_ip = ""
        # monotonic взятия лока (0 = свободен) — по нему вотчдог видит залипание.
        self._lock_since = 0.0
        # Дорожку надо пересоздать (её драйвер зависал) — сделает обслуживание.
        self._needs_relaunch = False
        # Обслуживание уже идёт (задача в полёте) — не запускать вторую.
        self._servicing = False
        self._last_at = 0.0          # monotonic последнего запроса
        self._last_warm = 0.0        # monotonic последнего успешного прогрева
        self._last_rotate = 0.0      # monotonic последней ротации
        self._warm_fails = 0
        self._next_warm = 0.0        # monotonic — раньше не перепрогревать (backoff)
        self._launched_at = 0.0      # monotonic создания браузера (метрика возраста)
        # Свой порог recycle у каждой дорожки (база ± джиттер) — чтобы пул не
        # пересоздавался разом. Считается один раз, при создании браузера.
        self._age_limit = 0.0
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
        # no_viewport=True: НЕ слать setDefaultViewport (playwright кладёт туда поле
        # isMobile, которого Firefox-juggler camoufox не знает → new_page падает
        # "isMobile ... not described in this scheme"). Размер окна держит сам
        # camoufox через фингерпринт, viewport от playwright тут лишний и вредный.
        self._page = await self._browser.new_page(no_viewport=True)
        await _add_cookies_safe(self._page.context, _cookie_jar(self.cookie))
        # Возраст браузера: память camoufox растёт по аптайму (плато ~5 ГиБ), а
        # СПОСОБНОСТЬ ПРОХОДИТЬ FAB с возрастом падает (см. LANE_MAX_AGE_S) —
        # поэтому браузер планово пересоздаётся, не дожидаясь ни OOM, ни тихой
        # деградации пула. Сдвиг плато по памяти по-прежнему видно по
        # ozon_miner_lane_age_seconds раньше, чем по OOM.
        self._launched_at = time.monotonic()
        self._age_limit = LANE_MAX_AGE_S * (1.0 + random.uniform(0.0, LANE_MAX_AGE_JITTER))

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
        self._needs_relaunch = False
        try:
            # Дедлайн: старт camoufox тоже умеет зависнуть (кончились ресурсы,
            # не поднялся Xvfb) — без него ремонт дорожки встаёт навсегда.
            await asyncio.wait_for(self._launch(), timeout=MAINT_OP_TIMEOUT_S)
            return True
        except asyncio.TimeoutError:
            log.error("дорожка %d: браузер не поднялся за %.0fс", self.idx, MAINT_OP_TIMEOUT_S)
            return False
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: пересоздание браузера упало: %s",
                      self.idx, _first_line(e))
            return False

    def _warm_backoff(self, reason: str):
        """Пометить дорожку нездоровой и отложить следующий прогрев (экспонента):
        не долбим FAB — именно частые неудачные прогревы и жгут IP."""
        self.healthy = False
        self._warm_fails += 1
        backoff = min(MAINT_INTERVAL_S * (2 ** self._warm_fails), WARM_BACKOFF_MAX_S)
        self._next_warm = time.monotonic() + backoff
        log.warning("дорожка %d: %s (попыток подряд %d) — backoff %.0fс",
                    self.idx, reason, self._warm_fails, backoff)

    async def warm(self):
        """Навигация на карточку + ожидание, что FAB пройден (тестовый fetch=200).
        Успех → healthy, сброс backoff. Неудача → экспоненциальный backoff."""
        # Браузера может не быть вовсе: start() упал (напр. Playwright не умеет
        # socks5 с авторизацией — "Browser does not support socks5 proxy
        # authentication"), а дорожка всё равно кладётся в пул. Без этой проверки
        # цикл ниже сыпал "'NoneType' object has no attribute 'evaluate'" каждые
        # 3с весь WARM_WAIT_S, забивая логи и маскируя настоящие ошибки.
        # _needs_relaunch ставит fetch_path, когда его дедлайн сработал: драйвер
        # подвис, ре-навигация по нему бессмысленна — нужен свежий браузер.
        if (self._page is None or self._needs_relaunch) and not await self._relaunch():
            self._warm_backoff("браузер не поднялся")
            return
        log.info("дорожка %d: прогрев — навигация на %s", self.idx, WARM_URL)
        nav_ok = True
        try:
            await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                  timeout=int(NAV_TIMEOUT_S * 1000))
        except Exception as e:  # noqa: BLE001
            nav_ok = False
            log.warning("дорожка %d: навигация прогрева: %s",
                        self.idx, _first_line(e))
            # Драйвер мёртв → пересоздать браузер и повторить навигацию один раз.
            if _is_dead(e) and await self._relaunch():
                try:
                    await self._page.goto(WARM_URL, wait_until="domcontentloaded",
                                          timeout=int(NAV_TIMEOUT_S * 1000))
                    nav_ok = True
                except Exception as e2:  # noqa: BLE001
                    log.warning("дорожка %d: навигация после пересоздания: %s",
                                self.idx, _first_line(e2))
        self.egress_ip = await self._egress_ip()
        # Навигацию не бросаем даже при ошибке: страница может быть жива (частичная
        # загрузка), и in-page fetch ниже иногда всё равно проходит. Но и «ок» тут
        # писать нельзя — на спалённом IP лог врал «навигация ок» после RST.
        log.info("дорожка %d: навигация %s (egress=%s), жду прохождения FAB…",
                 self.idx, "ок" if nav_ok else "НЕ прошла", self.egress_ip or "?")
        deadline = time.time() + WARM_WAIT_S
        warm_path = _product_path(WARM_PRODUCT_ID)
        while time.time() < deadline:
            status, body = await self._inpage_fetch(warm_path)
            if status == 200 and not _looks_blocked(status, body):
                self.healthy = True
                self._warm_fails = 0
                self._next_warm = 0.0
                self._last_warm = time.monotonic()
                _mark_success()
                log.info("дорожка %d прогрета: egress=%s, FAB пройден",
                         self.idx, self.egress_ip or "?")
                return
            await self._nudge()
            # Пауза — СВОИМ asyncio.sleep, а не page.wait_for_timeout: паузу
            # незачем гонять через драйвер, а на подвисшем драйвере она не
            # возвращается вовсе (ровно этим 29-07 залип пул).
            await asyncio.sleep(3)
        # не прогрелась — backoff, чтобы не долбить FAB (это и жжёт IP)
        self._warm_backoff("прогрев не дал 200")

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
        Ретраим ТОЛЬКО транзиентные пустышки (таймаут/0/иной не-200).

        ДЕДЛАЙН: вся работа под локом ограничена LANE_OP_TIMEOUT_S. Инцидент 29-07:
        подвисший драйвер держал лок вечно → запросы Go копились на входе в лок
        (p95 30с), обслуживание пропускало залоченную дорожку, пул стоял 11ч при
        healthy=4. Таймаут → 504 (Go считает это бедой сайдкара, а не FAB)."""
        async with self.lock:
            self._lock_since = time.monotonic()
            try:
                return await asyncio.wait_for(self._fetch_locked(path, label),
                                              timeout=LANE_OP_TIMEOUT_S)
            except asyncio.TimeoutError:
                self.healthy = False
                self._needs_relaunch = True
                _observe_latency("blocked", LANE_OP_TIMEOUT_S)
                log.error("дорожка %d: %s завис дольше %.0fс — нездорова, "
                          "браузер на пересоздание", self.idx, label, LANE_OP_TIMEOUT_S)
                return 504, b""
            finally:
                self._lock_since = 0.0

    async def _fetch_locked(self, path: str, label: str):
        """Тело fetch_path под уже взятым локом (вынесено, чтобы обернуть в
        wait_for целиком — включая спейсинг и паузы между ретраями)."""
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
                _mark_success()
                log.info("дорожка %d: %s ок за %.2fс (спейсинг %.2fс, попыток %d)",
                         self.idx, label, elapsed, waited, attempt)
                return 200, body.encode("utf-8")
            if _looks_blocked(status, body):
                break  # FAB — ретрай бесполезен, выходим быстро (см. docstring)
            if attempt < SCRAPE_RETRIES:  # транзиент — короткая пауза и ещё попытка
                await self._nudge()
                # Пауза СВОИМ sleep, не через драйвер: page.wait_for_timeout на
                # подвисшем драйвере не возвращается (корень дедлока 29-07).
                await asyncio.sleep(RETRY_PAUSE_MS / 1000.0)
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
        await _quiet_call(self._page.mouse.wheel(0, random.randint(200, 900)),
                          DRIVER_CALL_TIMEOUT_S, "нудж", self.idx)

    async def _egress_ip(self) -> str:
        # new_page() дедлайна НЕ имеет (в отличие от goto) — на подвисшем драйвере
        # висит вечно, а вызывается это из warm(), т.е. из обслуживающего цикла.
        try:
            return await asyncio.wait_for(self._egress_ip_inner(),
                                          timeout=DRIVER_CALL_TIMEOUT_S)
        except Exception:  # noqa: BLE001
            return ""

    async def _egress_ip_inner(self) -> str:
        p = None
        try:
            p = await self._browser.new_page(no_viewport=True)
            await p.goto("https://api.ipify.org?format=json", timeout=15000)
            txt = await p.evaluate("() => document.body.innerText")
            m = re.search(r'"ip":\s*"([^"]+)"', txt or "")
            return m.group(1) if m else ""
        except Exception:  # noqa: BLE001
            return ""
        finally:
            if p is not None:
                await _quiet_call(p.close(), 5, "закрытие egress-страницы", self.idx)

    def due_rotate(self, now: float) -> bool:
        # Без switch-ссылки менять НЕЧЕГО: rotate() пропустит смену IP и всё равно
        # дойдёт до warm(), т.е. выбросит прогретую сессию и заново пройдёт FAB с
        # ТОГО ЖЕ адреса. На проде так и было (интервал 30м при пустом ROTATE_URL,
        # все дорожки direct) — лишние челленджи FAB, а это и жжёт IP.
        # Живые сессии поддерживает keepalive, ротация — только под прокси.
        return (bool(ROTATE_URL) and ROTATE_INTERVAL_S > 0
                and (now - self._last_rotate) >= ROTATE_INTERVAL_S)

    def due_keepalive(self, now: float) -> bool:
        return (self.healthy and WARM_KEEPALIVE_S > 0
                and (now - self._last_warm) >= WARM_KEEPALIVE_S)

    def due_rewarm(self, now: float) -> bool:
        return (not self.healthy) and now >= self._next_warm

    def age(self, now: float) -> float:
        """Сколько секунд живёт текущий браузер (0 = ещё не создан)."""
        return (now - self._launched_at) if self._launched_at else 0.0

    def due_recycle(self, now: float) -> bool:
        """Пора планово пересоздать браузер: он зажился и перестаёт проходить FAB.
        Только для ЗДОРОВОЙ дорожки — нездоровую и так чинит due_rewarm, и там
        пересоздание уже своё (_needs_relaunch)."""
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
        # С дедлайном: __aexit__ на подвисшем драйвере не возвращается, а close()
        # зовётся из _relaunch() внутри обслуживания — зависнет весь ремонт пула.
        if self._cam:
            await _quiet_call(self._cam.__aexit__(None, None, None),
                              DRIVER_CALL_TIMEOUT_S, "закрытие браузера", self.idx)


# ── Пул ──────────────────────────────────────────────────────────────────────
class Pool:
    def __init__(self, lanes):
        self.lanes = lanes

    def pick(self, product_id: str):
        """Дорожка под обычный товар: ТОЛЬКО анонимная живая, с шардированием по
        product_id для липкости товар→дорожка. Нет анонимных → None (502), а НЕ
        фолбэк на authed: аккаунт нужен под 18+, и гонять по нему массовый поток
        значит менять обратимую потерю площадки на необратимый бан аккаунта."""
        pool = [l for l in self.lanes if l.healthy and not l.authed]
        if not pool:
            _mark_anon_starved()
            return None
        try:
            i = int(product_id) % len(pool)
        except ValueError:
            i = hash(product_id) % len(pool)
        return pool[i]

    def pick_authed(self):
        """Живая АВТОРИЗОВАННАЯ дорожка — для товаров 18+ (аноним упирается в
        возрастной гейт). None → нет живой authed-дорожки (18+ недоступны)."""
        alive = [l for l in self.lanes if l.healthy and l.authed]
        return random.choice(alive) if alive else None

    def pick_any(self):
        """Любая живая АНОНИМНАЯ дорожка (поиск/витрина — нет product_id для
        шардирования). Как и pick(), на authed не фолбэчит."""
        pool = [l for l in self.lanes if l.healthy and not l.authed]
        if not pool:
            _mark_anon_starved()
            return None
        return random.choice(pool)

    def healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy)

    def authed_healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy and l.authed)

    def anon_healthy_count(self) -> int:
        return sum(1 for l in self.lanes if l.healthy and not l.authed)

    def stuck_lanes(self, now: float) -> int:
        return sum(1 for l in self.lanes if l.stuck_for(now) > 0)

    async def maintenance_loop(self):
        """Раз в MAINT_INTERVAL_S: вотчдог залипших дорожек, ротация по расписанию,
        перепрогрев нездоровых (с backoff), keepalive-прогрев живых.

        Обслуживание КАЖДОЙ дорожки — отдельная задача с дедлайном: раньше цикл
        шёл последовательно и `await lane.warm()` на подвисшем драйвере морозил
        ремонт всего пула (инцидент 29-07: 11ч без единого перепрогрева)."""
        while True:
            await asyncio.sleep(MAINT_INTERVAL_S)
            now = time.monotonic()
            for lane in self.lanes:
                # Вотчдог: лок держат дольше LANE_STUCK_S → дорожка залипла.
                # Снимаем healthy (pick() её обойдёт) и метим на пересоздание —
                # иначе она числится живой и молча съедает трафик.
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
                    # healthy снимаем ЧЕСТНО: браузер сейчас исчезнет, отдавать
                    # на него запросы нельзя. Дальше идёт штатный путь
                    # _service_lane: _needs_relaunch → _relaunch() → due_rewarm →
                    # warm(). _next_warm обнуляем, чтобы прогрев пошёл сразу, а
                    # не после backoff, оставшегося от прошлых неудач.
                    log.info("дорожка %d: плановый recycle — браузер живёт %.1fч",
                             lane.idx, lane.age(now) / 3600.0)
                    lane.healthy = False
                    lane._needs_relaunch = True
                    lane._next_warm = 0.0
                elif not (lane.due_rotate(now) or lane.due_rewarm(now)
                          or lane.due_keepalive(now)):
                    continue
                # Флаг ставим ЗДЕСЬ, а не в задаче: между ensure_future и первой
                # строкой задачи цикл мог бы успеть завести вторую такую же.
                lane._servicing = True
                asyncio.ensure_future(self._service_lane(lane))

    async def _service_lane(self, lane):
        """Обслужить одну дорожку под её локом, с дедлайном на операцию."""
        try:
            async with lane.lock:
                lane._lock_since = time.monotonic()
                now = time.monotonic()
                if lane.due_rotate(now):
                    what, op = "плановая ротация IP", lane.rotate()
                elif lane.due_rewarm(now):
                    what, op = "нездорова — перепрогрев", lane.warm()
                elif lane.due_keepalive(now):
                    what, op = "keepalive-прогрев", lane.warm()
                else:
                    op = None
                if op is None:
                    return
                log.info("дорожка %d: %s", lane.idx, what)
                try:
                    await asyncio.wait_for(op, timeout=MAINT_OP_TIMEOUT_S)
                except asyncio.TimeoutError:
                    lane._warm_backoff(f"обслуживание не уложилось в {MAINT_OP_TIMEOUT_S:.0f}с")
                    lane._needs_relaunch = True
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: обслуживание упало: %s", lane.idx, e)
        finally:
            lane._lock_since = 0.0
            lane._servicing = False


# ── HTTP ─────────────────────────────────────────────────────────────────────
async def handle_scrape(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    product_id = (request.query.get("id") or "").strip()
    if not product_id.isdigit():
        return web.json_response({"error": "id must be numeric"}, status=400)
    # authed=1 → товар за возрастным гейтом 18+, нужна авторизованная дорожка
    # (Go зовёт этот путь ретраем после ErrAgeRestricted на анонимной дорожке).
    authed = request.query.get("authed") == "1"
    lane = pool.pick_authed() if authed else pool.pick(product_id)
    if lane is None:
        return web.Response(status=502,
                            text="no healthy authed lanes" if authed
                            else "no healthy anonymous lanes")
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
        return web.Response(status=502, text="no healthy anonymous lanes")
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
        return web.Response(status=502, text="no healthy anonymous lanes")
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
        return web.Response(status=502, text="no healthy anonymous lanes")
    status, body = await lane.fetch_path(path, f"page:{path[:60]}")
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body,
                        content_type="application/json",
                        headers={"X-Ozon-Lane": str(lane.idx)})


async def handle_health(request: web.Request) -> web.Response:
    pool: Pool = request.app["pool"]
    lanes = [{"idx": l.idx, "healthy": l.healthy, "authed": l.authed,
              "egress_ip": l.egress_ip} for l in pool.lanes]
    healthy = pool.healthy_count()
    return web.json_response(
        {"healthy": healthy, "authed_healthy": pool.authed_healthy_count(),
         "total": len(pool.lanes), "lanes": lanes},
        status=200 if healthy > 0 else 503)


def _cgroup_stat(field: str):
    """Поле из memory.stat (anon/file/...) или None."""
    try:
        with open(_CGROUP_STAT) as f:
            for line in f:
                k, _, v = line.partition(" ")
                if k == field:
                    return int(v)
    except Exception:  # noqa: BLE001
        return None
    return None


def _memory_metric_lines():
    """Расход, АНОНИМНАЯ часть и лимит cgroup. Имя ОБЩЕЕ для всех сайдкаров (не
    с префиксом площадки): различает их лейбл service из scrape-конфига, и тогда
    одного правила алерта хватает на все три. Нет cgroup v2 или лимита — строку
    не отдаём вовсе, чтобы не врать нулём.

    ЗАЧЕМ ОТДЕЛЬНО anon. memory.current включает страничный кэш, а он
    вытесняемый: контейнер спокойно стоит у самого потолка, ядро сбрасывает кэш,
    расход откатывается — и так по кругу, без всякой беды. У ozon-miner 03-09
    максимум за сутки был 5.99 ГиБ из 6 при полном здравии, и алерт на
    memory.current звенел впустую. Сколько осталось до OOM показывает ТОЛЬКО
    anon: убить процесс ядро может, лишь когда не влезает невытесняемое.
    Поэтому SidecarMemoryHigh считает по anon, а current остаётся для картины."""
    out = []
    cur = _cgroup_bytes(_CGROUP_CURRENT)
    anon = _cgroup_stat("anon")
    lim = _cgroup_bytes(_CGROUP_MAX)
    if cur is not None:
        out += ["# HELP sidecar_memory_bytes Расход памяти контейнера ВКЛЮЧАЯ вытесняемый кэш (cgroup v2)",
                "# TYPE sidecar_memory_bytes gauge",
                f"sidecar_memory_bytes {cur}"]
    if anon is not None:
        out += ["# HELP sidecar_memory_anon_bytes Анонимная (невытесняемая) память — она упирается в лимит и вызывает OOM",
                "# TYPE sidecar_memory_anon_bytes gauge",
                f"sidecar_memory_anon_bytes {anon}"]
    if lim is not None:
        out += ["# HELP sidecar_memory_limit_bytes Потолок памяти контейнера (mem_limit)",
                "# TYPE sidecar_memory_limit_bytes gauge",
                f"sidecar_memory_limit_bytes {lim}"]
    return out


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
        "# HELP ozon_miner_authed_lanes Число живых АВТОРИЗОВАННЫХ дорожек (0 = 18+ товары недоступны)",
        "# TYPE ozon_miner_authed_lanes gauge",
        f"ozon_miner_authed_lanes {pool.authed_healthy_count()}",
        # Массовый поток обслуживают ТОЛЬКО анонимные дорожки, поэтому 0 здесь =
        # Ozon не скрейпится вовсе, даже когда healthy_lanes>0 (жива одна authed).
        # На этом висит алерт: healthy_lanes==0 такую ситуацию не ловит.
        "# HELP ozon_miner_anon_lanes Число живых АНОНИМНЫХ дорожек (0 = массовый поток не обслуживается)",
        "# TYPE ozon_miner_anon_lanes gauge",
        f"ozon_miner_anon_lanes {pool.anon_healthy_count()}",
        "# HELP ozon_miner_anon_starved_total Отказов массовому потоку из-за отсутствия живой анонимной дорожки",
        "# TYPE ozon_miner_anon_starved_total counter",
        f"ozon_miner_anon_starved_total {_anon_starved_total}",
        "# HELP ozon_miner_stuck_lanes Дорожки с локом, занятым дольше порога (залипший драйвер)",
        "# TYPE ozon_miner_stuck_lanes gauge",
        f"ozon_miner_stuck_lanes {pool.stuck_lanes(time.monotonic())}",
        # healthy_lanes ВРЁТ при залипании драйвера: дорожка числится живой, но
        # ничего не отдаёт (инцидент 29-07 — 11ч простоя при healthy=4, critical-
        # алерт на ==0 промолчал). Возраст последнего успеха не врёт — алерт на нём.
        "# HELP ozon_miner_last_success_age_seconds Секунд с последнего успешного скрейпа/прогрева",
        "# TYPE ozon_miner_last_success_age_seconds gauge",
        f"ozon_miner_last_success_age_seconds {time.monotonic() - _last_success_at:.0f}",
        "# HELP ozon_miner_lane_healthy Здоровье конкретной дорожки (1=healthy, 0=нет)",
        "# TYPE ozon_miner_lane_healthy gauge",
    ]
    for l in pool.lanes:
        lines.append(
            f'ozon_miner_lane_healthy{{lane="{l.idx}",authed="{1 if l.authed else 0}"}} '
            f'{1 if l.healthy else 0}')
    lines += [
        # Память camoufox коррелирует с АПТАЙМОМ браузера, а не с числом дорожек.
        # 20-08-2026 у ali-miner такой рост увёл весь хост в OOM; здесь плато
        # ~5 ГиБ и лимит 6g, но метрика нужна, чтобы заметить сдвиг плато.
        "# HELP ozon_miner_lane_age_seconds Секунд с создания браузера дорожки",
        "# TYPE ozon_miner_lane_age_seconds gauge",
    ]
    _now = time.monotonic()
    for l in pool.lanes:
        lines.append(f'ozon_miner_lane_age_seconds{{lane="{l.idx}"}} {l.age(_now):.0f}')
    lines.extend(_latency_metric_lines())
    lines.extend(_memory_metric_lines())
    return web.Response(text="\n".join(lines) + "\n", content_type="text/plain")


async def main():
    configs = _load_lane_configs()
    if not configs:
        raise SystemExit("нет ни одной сконфигурённой дорожки: задай OZON_COOKIE "
                         "(дорожка 0) или OZON_LANE_<i>_COOKIE")
    n_authed = sum(1 for c in configs if "__Secure-access-token" in c["cookie"])
    rotate_on = bool(ROTATE_URL) and ROTATE_INTERVAL_S > 0
    if ROTATE_INTERVAL_S > 0 and not ROTATE_URL:
        log.warning("OZON_ROTATE_INTERVAL_MINUTES=%.0f задан, но OZON_PROXY_ROTATE_URL пуст "
                    "— ротация ВЫКЛЮЧЕНА (менять IP нечем). Сессии держит keepalive.",
                    ROTATE_INTERVAL_S / 60)
    log.info("старт ozon-miner: port=%d дорожек=%d (аноним=%d authed=%d, POOL_SIZE=%d) "
             "движок=camoufox ротация=%s keepalive=%.0fмин",
             PORT, len(configs), len(configs) - n_authed, n_authed, POOL_SIZE,
             f"{ROTATE_INTERVAL_S/60:.0f}мин" if rotate_on else "выкл",
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
