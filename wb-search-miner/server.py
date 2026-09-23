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

С 23-09-2026 это не подпорка под горячие запросы, а ЕДИНСТВЕННЫЙ путь к WB:
публичные хосты (card/u-card/search/u-search .wb.ru) закрыты 403 для всех, включая
домашний RU-IP, а фронт ушёл на __internal за тем же сайтом. Из HTTP-клиента туда
не попасть даже с валидной кукой и верным JA3 (curl и curl_cffi impersonate → 498).

Дорожка прогревается навигацией на страницу поиска нейтрального запроса (проходит
wbaas-стену), затем выдачу и цены достаём «методом друга»: in-page fetch к
__internal ИЗНУТРИ доверенного контекста, с заголовком `deviceid` — без него
даже прогретая страница получает 403, с ним 200 (значение произвольное).

Go-скрейпер зовёт:
  GET /search?query=<q>&sort=<s>&page=<n>  — выдача (пагинация работает);
  GET /card?nm=id1;id2;...                 — живые цены пачкой до CARD_BATCH_MAX.
Оба отдают СЫРОЙ JSON той же формы, что раньше давали публичные хосты
(wbSearchResponse), зеркаля upstream-статус.

Живучесть: джиттер интервала, backoff на стойкой стене (не долбить — жжёт IP),
периодический re-warm, пересоздание браузера после смерти драйвера И после N
неудачных прогревов подряд (свежий фингерпринт против залипания на document-498).
Живой browse в прогреве (скролл+dwell) снижает шанс докатиться до стены. Прокси НЕ
нужен (майнер доказал: direct с датацентр-IP проходит), но опционально
поддержан через WB_SEARCH_PROXY_URL / WB_LANE_<i>_PROXY.
"""

import asyncio
import json
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

# Запрос прогрева. ХОЛОДНЫЙ намеренно: wbaas мягче к редким запросам (весь наш
# диагноз — 403 на горячих, 200 на редких), а прогрев раз в 30 мин по горячему
# «телефон» сам провоцирует эскалацию до document-стены (эпизоды 05-07 и 04-08).
# Прогреву нужен только факт 200 на своём u-search — какой запрос, не важно.
WARM_QUERY = os.getenv("WB_WARM_QUERY", "капибара")
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
# Живая карточка. 23-09-2026 WB закрыл ВСЕ публичные хосты (card/u-card/search/
# u-search — 403 wbaas всем, включая домашний RU-IP), и цена осталась только за
# фронтом, на том же __internal. Форма ответа прежняя, nm принимает пачкой.
UCARD_PATH = os.getenv("WB_UCARD_PATH", "/__internal/u-card/cards/v4/list")
# Карточка по одному артикулу. Нужна там, где list МОЛЧА пропускает товар: у
# распроданных нет живого оффера, и в пачке они просто отсутствуют (проверено
# 23-09: один и тот же набор теряется во всех прогонах). detail же отдаёт их с
# totalQuantity=0 и пустой ценой — это честный OOS, а без него товар сваливался
# на архивную цену и выглядел «в наличии», хотя его нет.
UCARD_DETAIL_PATH = os.getenv("WB_UCARD_DETAIL_PATH", "/__internal/u-card/cards/v4/detail")
# Сколько потерянных артикулов добирать поштучно. Дороже пачки, поэтому с
# потолком: остальные уедут на архив, как раньше.
CARD_DETAIL_MAX = int(os.getenv("WB_CARD_DETAIL_MAX", "4"))
# Сколько артикулов класть в один запрос карточек. Фронт сам шлёт по 10.
CARD_BATCH_MAX = int(os.getenv("WB_CARD_BATCH_MAX", "10"))
# Окно накопления пачки. Плата — задержка ответа, выигрыш — во столько раз
# меньше запросов к wbaas, сколько артикулов склеилось. Скрейп не интерактивен,
# четверть секунды тут никто не замечает.
CARD_BATCH_WINDOW_S = float(os.getenv("WB_CARD_BATCH_WINDOW_MS", "250")) / 1000.0
# Сколько ждать здоровую дорожку, прежде чем отдать отказ: перепрогрев после
# 498 занимает секунды, а отказ стоит товару архивной цены.
# Сколько ждать здоровую дорожку. Было 12с и две попытки — в сумме с заходом в
# публичный хост это давало p95 в 30с и душило ОБЩИЙ пул воркеров скрейпера, а
# следом и соседние площадки. Отказ должен стоить секунды: цена всё равно уедет
# в архив, и лучше сделать это быстро.
CARD_WAIT_LANE_S = float(os.getenv("WB_CARD_WAIT_LANE_SECONDS", "4"))
# Заголовок deviceid — ключ ко ВСЕМУ __internal (23-09-2026): без него in-page
# fetch из прогретой страницы даёт 403, с ним 200. Значение произвольное
# (site_<32 hex>), к сессии не привязано; x-spa-version/x-requested-with не нужны.
DEVICE_ID_PREFIX = os.getenv("WB_DEVICE_ID_PREFIX", "site_")
# Страница-стоянка. После прогрева дорожка НЕ должна оставаться на выдаче: это
# тяжёлая SPA, которая крутит таймеры и анимации круглосуточно — 23-09 вечером
# три дорожки съели 477% CPU, и в голоде оказались все площадки разом (p95 у WB,
# Ozon и Яндекса уехал к 30с одинаково). Лёгкий текстовый документ ТОГО ЖЕ
# origin сохраняет куки и доверие: in-page fetch к __internal с него отдаёт 200
# и по цене, и по поиску (проверено). about:blank не годится — origin теряется.
PARK_URL = os.getenv("WB_PARK_URL", "https://www.wildberries.ru/robots.txt")

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

# Плановое пересоздание браузера по ВОЗРАСТУ. Память дорожки растёт по аптайму:
# 20-08-2026 у ali-miner chrome дорос до 6.7 ГиБ и увёл ВЕСЬ хост в OOM.
# Здесь механизм тот же, но ВЫКЛЮЧЕН по умолчанию: прогрев дорого стоит —
# браузеру заново проходить wbaas-стену (create-token 498), и дёргать это по
# таймеру без нужды не стоит. Метрика wb_search_miner_lane_age_seconds пишется
# всегда — по ней и решать, нужно ли включать (>0 = порог в секундах).
LANE_MAX_AGE_S = float(os.getenv("WB_LANE_MAX_AGE_SECONDS", "0"))
LANE_MAX_AGE_JITTER = float(os.getenv("WB_LANE_MAX_AGE_JITTER", "0.2"))

MAINT_INTERVAL_S = float(os.getenv("WB_HEALTH_INTERVAL_SECONDS", "30"))
# Лок дорожки держат дольше этого — считаем её залипшей. Порог с запасом над
# самым долгим штатным запросом (навигация + ожидание u-search).
LANE_STUCK_S = float(os.getenv("WB_LANE_STUCK_SECONDS", "180"))
# Дедлайн на операцию обслуживания: без него `await warm()` на подвисшем
# драйвере морозит ремонт всего пула.
MAINT_OP_TIMEOUT_S = float(os.getenv("WB_MAINT_OP_TIMEOUT_SECONDS", "240"))
# Дедлайн на чтение тела ответа: единственный await в fetch_search, у которого
# своего тайм-аута нет, — именно на нём дорожка и вставала.
BODY_TIMEOUT_S = float(os.getenv("WB_BODY_TIMEOUT_SECONDS", "30"))
# Запас поверх собственного таймера Playwright у навигации: своя граница нужна
# на случай мёртвого CDP-соединения, когда таймер драйвера не срабатывает.
# Вынесен в параметр, иначе нижнюю границу ожидания не проверить тестом.
NAV_HARD_SLACK_S = float(os.getenv("WB_NAV_HARD_SLACK_SECONDS", "10"))
# Дедлайн на необязательные действия мыши.
NUDGE_TIMEOUT_S = float(os.getenv("WB_NUDGE_TIMEOUT_SECONDS", "15"))
WARM_KEEPALIVE_S = float(os.getenv("WB_WARM_KEEPALIVE_MINUTES", "30")) * 60.0
# Потолок backoff. 30 мин, а не 10: под стеной каждая попытка прогрева — это ~160
# челлендж-запросов, и долбёжка раз в 10 мин держит IP горячим в глазах wbaas
# (стена всё равно уходит по своему таймеру, а не от наших ретраев). Плата —
# снятие стены замечаем с задержкой до получаса.
WARM_BACKOFF_MAX_S = float(os.getenv("WB_WARM_BACKOFF_MAX_SECONDS", "1800"))
# Сколько неудач подряд считаем «стоит стена»: дальше прогреваем ОДНОЙ навигацией
# вместо WARM_RELOADS — вторая под стеной даёт тот же 498 вдвое дороже.
WALLED_AFTER = int(os.getenv("WB_WALLED_AFTER", "2"))
# После скольких ПОДРЯД неудачных прогревов пересоздать браузер (свежий контекст/
# фингерпринт) вместо долбёжки того же контекста. Лечит залипание на document-498:
# когда wbaas walled сам HTML навигации, ре-навигация той же сессии часами даёт
# тот же 498 (наблюдалось ~6ч под тестовой молотилкой горячих). 0 — не пересоздавать.
WARM_RELAUNCH_AFTER = int(os.getenv("WB_WARM_RELAUNCH_AFTER", "3"))

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
    r"Browser.*closed|has been closed|Target closed|"
    # «Page crashed»/«Target crashed» — рендерер убит (обычно OOM в cgroup).
    # См. разбор в ali-miner/server.py: без этого дорожка лечится минутами.
    r"crashed",
    re.IGNORECASE)


# Момент последнего УСПЕХА пула (успешный запрос или прогрев). Гонится в метрику
# wb_search_miner_last_success_age_seconds: healthy_lanes врёт при залипании
# (дорожка числится живой, но не работает), а этот возраст — нет.
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

# Ключ фильтра выдачи WB: именованный (priceU, xsubject, fbrand…) или числовой
# фасет f5023. Белый список — чтобы через параметр filters нельзя было подставить
# в навигацию произвольный кусок URL.
_FILTER_KEY_RE = re.compile(r"^(?:priceU|dprice|xsubject|subject|fbrand|fsupplier|"
                            r"fcolor|fdlvr|fkind|frating|foriginal|f\d+)$")
# Значение фильтра: id-шники, диапазоны цен, списки через «;».
_FILTER_VAL_RE = re.compile(r"^[0-9A-Za-z;,._-]{1,200}$")


def _sanitize_filters(raw: str) -> str:
    """Отфильтровать кусок query WB («priceU=1;2&xsubject=3») до известных пар."""
    out = []
    for pair in (raw or "").split("&"):
        if not pair or "=" not in pair:
            continue
        k, v = pair.split("=", 1)
        v = unquote(v)
        if _FILTER_KEY_RE.match(k) and _FILTER_VAL_RE.match(v):
            out.append(k + "=" + quote(v, safe=";,"))
    return "&".join(out)


def _search_page_url(query: str, sort: str, page: int, filters: str = "") -> str:
    """URL страницы поиска для навигации (фронт сам дёрнет u-search). sort/page и
    фильтры (цена/предмет/бренд) — штатные query-параметры каталога WB: без них
    фронт запросит у u-search ГОЛУЮ выдачу, а не то, что выбрал пользователь."""
    url = SEARCH_PAGE_URL.format(query=quote(query))
    extra = []
    if sort and sort != "popular":
        extra.append("sort=" + quote(sort))
    if page and page > 1:
        extra.append("page=" + str(page))
    if filters:
        extra.append(filters)
    if extra:
        url += "&" + "&".join(extra)
    return url


def _new_device_id() -> str:
    """deviceid вида site_<32 hex> — тот самый заголовок, без которого __internal
    отвечает 403 даже прогретому браузеру (23-09-2026). Значение произвольное:
    фронт генерит его сам, привязки к сессии/куке нет — проверено случайным."""
    return DEVICE_ID_PREFIX + "%032x" % random.getrandbits(128)


def _ucard_url(nms: str) -> str:
    """Относительный URL живых карточек пачкой: nm=id1;id2;... (та же форма
    ответа, что у закрытого u-card.wb.ru — products[].sizes[].price)."""
    return (UCARD_PATH + "?appType=1&curr=rub&dest=" + quote(WB_DEST)
            + "&spp=" + quote(WB_SPP) + "&lang=ru&ab_testing=false&nm=" + quote(nms))


def _ucard_detail_url(nm: str) -> str:
    """URL карточки одного артикула (отдаёт и распроданный товар — с qty 0)."""
    return (UCARD_DETAIL_PATH + "?appType=1&curr=rub&dest=" + quote(WB_DEST)
            + "&spp=" + quote(WB_SPP) + "&mtype=257&lang=ru&ab_testing=false&nm="
            + quote(nm))


def _search_api_url(query: str, sort: str, page: int) -> str:
    """Относительный URL выдачи для in-page fetch. Пагинация здесь РАБОТАЕТ
    (в отличие от навигации по &page=N, которая всегда отдавала стр. 1)."""
    url = (USEARCH_PATH + "?appType=1&curr=rub&dest=" + quote(WB_DEST)
           + "&spp=" + quote(WB_SPP) + "&query=" + quote(query)
           + "&resultset=catalog&lang=ru&locale=ru&suppressSpellcheck=false")
    url += "&sort=" + quote(sort or "popular")
    if page and page > 1:
        url += "&page=" + str(page)
    return url


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
        # deviceid — ключ к __internal (см. DEVICE_ID_PREFIX). Свой на дорожку,
        # чтобы дорожки не выглядели одним устройством.
        self.device_id = _new_device_id()

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
                # Корректный async-хендлер: блокируем ТОЛЬКО image/media (как
                # token-miner). Шрифты НЕ трогаем — их отсутствие меняет
                # canvas/font-fingerprint, wbaas это палит (create-token 498).
                # Обязательно await — fire-and-forget рушил бы загрузку/челлендж.
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
        # Свежий контекст — свежее «устройство»: старый deviceid мог попасть под
        # ограничение вместе с залипшей сессией.
        self.device_id = _new_device_id()
        try:
            await self._launch()
            return True
        except Exception as e:  # noqa: BLE001
            log.error("дорожка %d: пересоздание упало: %s", self.idx, e)
            return False

    async def _nudge(self):
        # Как _human_nudge в token-miner: движение мыши (steps) + колесо. wbaas
        # проверяет наличие человекоподобных событий указателя.
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
        # Более «живое» поведение на выдаче: несколько прокруток с паузами +
        # движения указателя. wbaas смотрит на dwell и события скролла/мыши, так
        # что живой browse снижает шанс докатиться до стены на холодную. ВАЖНО:
        # уже выданную document-стену (498 на самой навигации) это НЕ снимает —
        # страницы нет, скроллить нечего; это профилактика, а не лечение.
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
        """Прогрев как в token-miner: навигация на страницу поиска, СЛУШАЕМ
        ответы — ждём 200 на СОБСТВЕННОМ u-search XHR страницы (= wbaas-стена
        пройдена, cookie x_wbaas_token выставлен). Потом проверяем in-page fetch.
        Успех → healthy; неудача → экспоненциальный backoff."""
        # Залипание: N неудач подряд → текущий контекст, скорее всего, walled на
        # уровне document (498 на самой навигации, ре-навигация не помогает).
        # Пересоздаём браузер со свежим фингерпринтом ПЕРЕД прогревом.
        if WARM_RELAUNCH_AFTER > 0 and self._fails_since_relaunch >= WARM_RELAUNCH_AFTER:
            log.warning("дорожка %d: %d неудач подряд — пересоздаю браузер перед прогревом",
                        self.idx, self._fails_since_relaunch)
            if await self._relaunch():
                self._fails_since_relaunch = 0

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
        # Под стеной не платим за вторую навигацию: она даёт тот же document-498.
        reloads = 1 if self._warm_fails >= WALLED_AFTER else WARM_RELOADS
        per_attempt = max(30.0, WARM_WAIT_S / max(1, WARM_RELOADS))
        log.info("дорожка %d: прогрев — навигация на %s (попыток %d)", self.idx, url, reloads)
        try:
            for attempt in range(1, reloads + 1):
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
                log.info("дорожка %d: попытка %d, 200 нет (u-search: %s)",
                         self.idx, attempt, seen["statuses"][-6:] or "—")
        finally:
            try:
                self._page.remove_listener("response", on_resp)
            except Exception:  # noqa: BLE001
                pass

        # Стена пройдена, когда САМА страница получила 200 на своём u-search XHR
        # (cookie x_wbaas_token выставлен). Обслуживаем перехватом нативного ответа
        # (ручной fetch к u-search wbaas отвергает 403 — фронт кладёт что-то своё).
        if seen["ok"]:
            self.healthy = True
            self._warm_fails = 0
            self._fails_since_relaunch = 0
            self._next_warm = 0.0
            self._last_warm = time.monotonic()
            _mark_success()
            await self._park()
            log.info("дорожка %d прогрета: u-search 200 (wbaas пройден)", self.idx)
            return

        self.healthy = False
        self._warm_fails += 1
        self._fails_since_relaunch += 1
        backoff = min(MAINT_INTERVAL_S * (2 ** self._warm_fails), WARM_BACKOFF_MAX_S)
        self._next_warm = time.monotonic() + backoff
        log.warning("дорожка %d: прогрев не дал 200 (подряд %d, u-search: %s) — backoff %.0fс",
                    self.idx, self._warm_fails, seen["statuses"][-6:] or "—", backoff)
        # Полный дамп — только на первых неудачах: под многочасовой стеной он
        # одинаков и топит логи (~50 строк на попытку). Дальше короткая сводка.
        if self._warm_fails <= WALLED_AFTER:
            await self._dump_diag(seen)
        else:
            await self._dump_diag_short(seen)

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

    async def _dump_diag_short(self, seen: dict):
        """Одна строка вместо полного дампа: статусы document + заголовок страницы.
        Хватает, чтобы отличить «стоит стена» от «что-то изменилось»."""
        try:
            docs = [st for (rt, st, _u) in seen["all"] if rt == "document"]
            title = ""
            try:
                title = await self._page.title()
            except Exception:  # noqa: BLE001
                pass
            log.warning("дорожка %d: стена держится — ответов %d, document %s, title=%r",
                        self.idx, len(seen["all"]), docs[-4:] or "—", title)
        except Exception as e:  # noqa: BLE001
            log.warning("краткая диагностика упала: %s", _first_line(e))

    async def _park(self):
        """Увести дорожку с выдачи на лёгкую страницу того же origin. Куки и
        пройденный челлендж остаются при ней, а рендерер перестаёт жечь CPU."""
        try:
            await asyncio.wait_for(
                self._page.goto(PARK_URL, wait_until="domcontentloaded",
                                timeout=int(NAV_TIMEOUT_S * 1000)),
                timeout=NAV_TIMEOUT_S + NAV_HARD_SLACK_S)
        except Exception as e:  # noqa: BLE001
            # Не повод считать дорожку больной: прогрев уже удался, fetch
            # работает и с выдачи — просто дороже.
            log.warning("дорожка %d: парковка не удалась: %s", self.idx, _first_line(e))

    async def _api_fetch(self, rel_url: str, what: str):
        """In-page fetch к __internal ИЗ прогретой страницы, с заголовком
        deviceid. Дёшево (нет навигации) и работает для обеих ручек. Возвращает
        (status, body_bytes); 0 — дорожка не ответила (драйвер мёртв/висит)."""
        js = """async ([u, dev]) => {
            const r = await fetch(u, {headers: {deviceid: dev}, credentials: 'include'});
            return {s: r.status, b: await r.text()};
        }"""
        try:
            res = await asyncio.wait_for(
                self._page.evaluate(js, [rel_url, self.device_id]),
                timeout=FETCH_TIMEOUT_S)
        except asyncio.TimeoutError:
            self.healthy = False
            self._needs_relaunch = True
            log.warning("дорожка %d: %s не вернулся за %.0fс — нездорова",
                        self.idx, what, FETCH_TIMEOUT_S)
            return 0, b""
        except Exception as e:  # noqa: BLE001
            if _is_dead(e):
                self.healthy = False
                self._needs_relaunch = True
            log.warning("дорожка %d: %s упал: %s", self.idx, what, _first_line(e))
            return 0, b""
        status = int(res.get("s") or 0)
        body = (res.get("b") or "").encode("utf-8")
        if status != 200:
            # 403 здесь = прогрев протух (кука/челлендж), а не «нет товара».
            self.healthy = False
            log.warning("дорожка %d: %s status=%s — нездорова", self.idx, what, status)
        return status, body

    async def _spacing(self):
        """Человекоподобная пауза между запросами одной дорожки."""
        t0 = time.monotonic()
        spacing = max(0.1, LANE_MIN_INTERVAL_S * (1.0 + LANE_JITTER * (2 * random.random() - 1)))
        wait = spacing - (t0 - self._last_at)
        if wait > 0:
            await asyncio.sleep(wait)
        self._last_at = time.monotonic()

    async def fetch_card(self, nms: str):
        """Живые карточки пачкой (nm=id1;id2;...). Ответ той же формы, что отдавал
        закрытый u-card.wb.ru, — Go-парсер не меняется."""
        async with self.lock:
            self._lock_since = time.monotonic()
            try:
                await self._spacing()
                status, body = await self._api_fetch(_ucard_url(nms), "u-card")
                if status == 200:
                    _mark_success()
                    log.info("дорожка %d: card %s ок (%d байт)",
                             self.idx, nms[:60], len(body))
                return status, body
            finally:
                self._lock_since = 0.0

    async def fetch_card_detail(self, nm: str):
        """Карточка одного артикула через detail — добор того, что list потерял."""
        async with self.lock:
            self._lock_since = time.monotonic()
            try:
                await self._spacing()
                status, body = await self._api_fetch(_ucard_detail_url(nm), "u-card detail")
                if status == 200:
                    _mark_success()
                return status, body
            finally:
                self._lock_since = 0.0

    async def fetch_search(self, query: str, sort: str, page: int, filters: str = ""):
        """Выдача из прогретого браузера. Основной путь — in-page fetch к
        u-search (дёшево, и пагинация работает). Фильтры каталога через API не
        переносятся, поэтому с ними идём прежним путём: навигация на страницу
        запроса и перехват нативного ответа фронта."""
        if not filters:
            async with self.lock:
                self._lock_since = time.monotonic()
                try:
                    await self._spacing()
                    status, body = await self._api_fetch(
                        _search_api_url(query, sort, page), "u-search")
                    if status == 200:
                        _mark_success()
                        log.info("дорожка %d: search %r p%d ок (%d байт)",
                                 self.idx, query[:40], page, len(body))
                        return status, body
                finally:
                    self._lock_since = 0.0
            # Сюда попадаем при 403/пустом ответе: пробуем прежний путь через
            # навигацию — он переживает протухший deviceid-путь.
        return await self._fetch_search_via_nav(query, sort, page, filters)

    async def _fetch_search_via_nav(self, query: str, sort: str, page: int, filters: str = ""):
        """Навигируем прогретый браузер на страницу запроса и ПЕРЕХВАТЫВАЕМ ответ
        u-search, который фронт делает сам (нативно, со всеми нужными заголовками —
        ручной fetch wbaas отвергает 403). Возвращает (status, body_bytes)."""
        nav_url = _search_page_url(query, sort, page, filters)
        async with self.lock:
            self._lock_since = time.monotonic()
            await self._spacing()

            loop = asyncio.get_event_loop()
            fut: asyncio.Future = loop.create_future()

            def on_resp(resp):
                try:
                    if USEARCH_MARKER in resp.url and not fut.done():
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
                    # u-search часто прилетает ещё до полной загрузки — навигационную
                    # ошибку не считаем фаталом, ждём ответ ниже.
                    if _is_dead(e):
                        self.healthy = False
                        return 502, b""
                # лёгкий нудж — иногда выдача подгружается после взаимодействия
                await self._nudge()
                try:
                    resp = await asyncio.wait_for(fut, timeout=FETCH_TIMEOUT_S)
                except asyncio.TimeoutError:
                    self.healthy = False
                    log.warning("дорожка %d: u-search не прилетел на %r p%d — нездорова",
                                self.idx, query[:40], page)
                    return 403, b""
                status = resp.status
                # С дедлайном: единственный await без своего тайм-аута. Тело
                # может не дойти никогда, а корутина держит лок дорожки — и
                # тогда пул стоит при зелёном healthy (инцидент 13-08).
                try:
                    body = await asyncio.wait_for(resp.body(), timeout=BODY_TIMEOUT_S)
                except asyncio.TimeoutError:
                    self.healthy = False
                    self._needs_relaunch = True
                    log.warning("дорожка %d: тело u-search не дошло за %.0fс — нездорова",
                                self.idx, BODY_TIMEOUT_S)
                    return 504, b""
                except Exception as e:  # noqa: BLE001
                    log.warning("дорожка %d: чтение тела u-search упало: %s", self.idx, _first_line(e))
                    return 502, b""
                if status == 200:
                    _mark_success()
                    log.info("дорожка %d: search %r p%d ок (%d байт)",
                             self.idx, query[:40], page, len(body))
                    return 200, body
                self.healthy = False
                log.warning("дорожка %d: u-search status=%s на %r p%d — нездорова",
                            self.idx, status, query[:40], page)
                return status, body
            finally:
                self._lock_since = 0.0
                try:
                    self._page.remove_listener("response", on_resp)
                except Exception:  # noqa: BLE001
                    pass
                # Со страницы выдачи уходим сразу: держать её открытой дорого.
                await self._park()

    def due_keepalive(self, now: float) -> bool:
        return self.healthy and WARM_KEEPALIVE_S > 0 and (now - self._last_warm) >= WARM_KEEPALIVE_S

    def due_rewarm(self, now: float) -> bool:
        return (not self.healthy) and now >= self._next_warm

    def age(self, now: float) -> float:
        """Сколько секунд живёт текущий браузер (0 = ещё не создан)."""
        return (now - self._launched_at) if self._launched_at else 0.0

    def due_recycle(self, now: float) -> bool:
        """Пора планово пересоздать браузер (см. LANE_MAX_AGE_S; 0 = выключено).
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

    async def pick_wait(self, timeout: float):
        """Дождаться здоровой дорожки. Перепрогрев после 498 занимает секунды, и
        ждать его дешевле, чем отдать отказ: на отказе цена товара уезжает в
        архив, отставший на дни."""
        deadline = time.monotonic() + timeout
        while True:
            lane = self.pick()
            if lane is not None or time.monotonic() >= deadline:
                return lane
            await asyncio.sleep(0.25)

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
        живой и молча съедала все запросы (тот же отказ, что у ozon-miner 29-07).
        """
        while True:
            await asyncio.sleep(MAINT_INTERVAL_S)
            now = time.monotonic()
            for lane in self.lanes:
                # Вотчдог: лок держат дольше LANE_STUCK_S → дорожка залипла.
                # Снимаем healthy (pick() её обойдёт) и метим на пересоздание.
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
                    # healthy снимаем ЧЕСТНО: браузер сейчас исчезнет. Дальше
                    # штатный путь _service_lane: _relaunch() → due_rewarm → warm().
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
                # Залипшую дорожку сперва пересоздаём: контекст, на котором
                # зависла операция, прогревать бессмысленно.
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
    query = (request.query.get("query") or "").strip()
    if not query:
        return web.json_response({"error": "query required"}, status=400)
    sort = (request.query.get("sort") or "popular").strip()
    try:
        page = max(1, int(request.query.get("page") or "1"))
    except ValueError:
        page = 1
    filters = _sanitize_filters(request.query.get("filters") or "")
    lane = pool.pick()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_search(query, sort, page, filters)
    if status == 0:
        return web.Response(status=502, text="lane fetch failed")
    return web.Response(status=status, body=body, content_type="application/json",
                        headers={"X-WB-Lane": str(lane.idx)})


class CardBatcher:
    """Склейка одиночных запросов цены в пачки.

    Go зовёт цену по одному товару, а таких товаров тысячи за цикл — и каждый
    запрос тратит доверие дорожки: 23-09 при потоке «по одному» wbaas выдавал
    498 каждые 2–4 минуты, дорожка уходила в перепрогрев, и цены сваливались в
    архив (живых оставалась пятая часть). Фронт WB сам запрашивает карточки
    пачками по 10 — делаем так же: ждём короткое окно, склеиваем накопившиеся
    артикулы в один запрос и раздаём каждому свой товар. Нагрузка на wbaas
    падает во столько раз, сколько артикулов попало в пачку.
    """

    def __init__(self, pool: "Pool"):
        self.pool = pool
        self._waiting: dict[str, list[asyncio.Future]] = {}
        self._wake = asyncio.Event()

    async def get(self, nm: str):
        """(status, product|None) для одного артикула."""
        fut = asyncio.get_event_loop().create_future()
        self._waiting.setdefault(nm, []).append(fut)
        self._wake.set()
        return await fut

    async def loop(self):
        while True:
            await self._wake.wait()
            # Окно накопления: за это время подтянутся соседние запросы.
            await asyncio.sleep(CARD_BATCH_WINDOW_S)
            batch, futures = {}, []
            for nm in list(self._waiting)[:CARD_BATCH_MAX]:
                batch[nm] = self._waiting.pop(nm)
                futures.extend(batch[nm])
            if not self._waiting:
                self._wake.clear()
            if not batch:
                continue
            try:
                await self._serve(batch)
            except Exception as e:  # noqa: BLE001
                log.error("батчер карточек упал: %s", _first_line(e))
                for fs in batch.values():
                    for f in fs:
                        if not f.done():
                            f.set_result((502, None))

    async def _serve(self, batch: dict):
        nms = ";".join(batch)
        status, body = 502, b""
        # Две попытки: 498 = у дорожки протух токен, сайдкар метит её нездоровой
        # и чинит за секунды — вторая попытка идёт уже по здоровой.
        for attempt in (1, 2):
            lane = await self.pool.pick_wait(CARD_WAIT_LANE_S)
            if lane is None:
                status, body = 502, b""
                break
            status, body = await lane.fetch_card(nms)
            if status == 200:
                break
        products = {}
        if status == 200:
            try:
                for p in json.loads(body or b"{}").get("products") or []:
                    products[str(p.get("id"))] = p
            except Exception as e:  # noqa: BLE001
                log.warning("батчер: ответ не разобран: %s", _first_line(e))
                status = 502
        # Кого пачка потеряла — добираем поштучно: у распроданных нет оффера, и
        # list их не отдаёт вовсе. Без этого товар уезжал на архивную цену и
        # числился в наличии, хотя его нет.
        if status == 200:
            missing = [nm for nm in batch if nm not in products]
            for nm in missing[:CARD_DETAIL_MAX]:
                lane = await self.pool.pick_wait(CARD_WAIT_LANE_S)
                if lane is None:
                    break
                st, body = await lane.fetch_card_detail(nm)
                if st != 200:
                    continue
                try:
                    for p in json.loads(body or b"{}").get("products") or []:
                        products[str(p.get("id"))] = p
                except Exception as e:  # noqa: BLE001
                    log.warning("батчер: detail не разобран: %s", _first_line(e))

        for nm, fs in batch.items():
            for f in fs:
                if not f.done():
                    f.set_result((status, products.get(nm)))


async def handle_card(request: web.Request) -> web.Response:
    """GET /card?nm=id1;id2;... — живые карточки (цена, наличие) из браузера.
    Артикулов не больше CARD_BATCH_MAX: столько же кладёт в запрос сам фронт."""
    pool: Pool = request.app["pool"]
    raw = (request.query.get("nm") or "").strip()
    nms = [x for x in re.split(r"[;,\s]+", raw) if x.isdigit()]
    if not nms:
        return web.json_response({"error": "nm required"}, status=400)
    if len(nms) > CARD_BATCH_MAX:
        return web.json_response(
            {"error": "too many nm (max %d)" % CARD_BATCH_MAX}, status=400)
    if len(nms) == 1:
        # Штатный путь: Go зовёт по одному товару, батчер склеит их в пачку.
        status, product = await request.app["cards"].get(nms[0])
        if status != 200:
            return web.Response(status=status or 502, text="card fetch failed")
        return web.json_response({"products": [product] if product else []})

    # Пачка артикулов в запросе — ручная проверка; идём напрямую, без окна.
    lane = pool.pick()
    if lane is None:
        return web.Response(status=502, text="no healthy lanes")
    status, body = await lane.fetch_card(";".join(nms))
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
    now = time.monotonic()
    lines += [
        # Главная метрика живости: healthy_lanes врёт при залипании драйвера
        # (дорожка числится живой, но не отвечает), а возраст успеха — нет.
        "# HELP wb_search_miner_last_success_age_seconds Секунд с последнего успешного запроса или прогрева",
        "# TYPE wb_search_miner_last_success_age_seconds gauge",
        f"wb_search_miner_last_success_age_seconds {now - _last_success_at:.0f}",
        # Возраст браузера: растущая память коррелирует именно с ним.
        "# HELP wb_search_miner_lane_age_seconds Секунд с создания браузера дорожки",
        "# TYPE wb_search_miner_lane_age_seconds gauge",
    ] + [
        f'wb_search_miner_lane_age_seconds{{lane="{l.idx}"}} {l.age(now):.0f}'
        for l in pool.lanes
    ] + [
        "# HELP wb_search_miner_stuck_lanes Дорожек с локом, занятым дольше порога",
        "# TYPE wb_search_miner_stuck_lanes gauge",
        f"wb_search_miner_stuck_lanes {pool.stuck_lanes(now)}",
    ]
    lines.extend(_memory_metric_lines())
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
        cards = CardBatcher(pool)
        app["cards"] = cards
        app.router.add_get("/search", handle_search)
        app.router.add_get("/card", handle_card)
        app.router.add_get("/healthz", handle_health)
        app.router.add_get("/metrics", handle_metrics)

        asyncio.ensure_future(pool.maintenance_loop())
        asyncio.ensure_future(cards.loop())

        runner = web.AppRunner(app)
        await runner.setup()
        site = web.TCPSite(runner, "0.0.0.0", PORT)
        await site.start()
        log.info("слушаю :%d (живых дорожек %d/%d)", PORT, pool.healthy_count(), len(lanes))

        while True:
            await asyncio.sleep(3600)


if __name__ == "__main__":
    asyncio.run(main())
