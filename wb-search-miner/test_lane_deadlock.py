#!/usr/bin/env python3
"""
Регрессия на залипание дорожки — тот же класс аварии, что закрыт в ozon-miner
после инцидента 29-07-2026 и найден здесь 13-08-2026: GET /search не отвечал
120с при healthy_lanes=1, и браузерный фолбэк на 403 фактически не работал,
пока мониторинг показывал зелёное.

Тест не поднимает браузер: подменяет страницу заглушкой, которая ВИСНЕТ на
любом await. Проверяем четыре свойства, которых у старого кода не было:
  A. fetch_search на висящем драйвере возвращается по дедлайну, лок отпущен;
  B. вотчдог снимает healthy с дорожки, чей лок держат дольше порога;
  C. зависшее обслуживание одной дорожки не мешает обслуживать остальные;
  D. возраст последнего успеха растёт (метрика, на которой висит алерт).

Запуск: python3 wb-search-miner/test_lane_deadlock.py
Зависимостей нет — aiohttp/patchright замоканы.
"""
import asyncio
import os
import sys
import types
from pathlib import Path

# ── Пороги делаем маленькими ДО импорта server.py (константы читаются на импорте)
os.environ.update({
    "WB_SEARCH_POOL_SIZE": "2",
    "WB_FETCH_TIMEOUT_SECONDS": "0.3",
    "WB_NAV_TIMEOUT_SECONDS": "0.3",
    "WB_NAV_HARD_SLACK_SECONDS": "0.2",
    "WB_BODY_TIMEOUT_SECONDS": "0.3",
    "WB_NUDGE_TIMEOUT_SECONDS": "0.3",
    "WB_MAINT_OP_TIMEOUT_SECONDS": "1",
    "WB_LANE_STUCK_SECONDS": "1",
    "WB_HEALTH_INTERVAL_SECONDS": "0.2",
    "WB_LANE_MIN_INTERVAL_MS": "0",
    "LOG_LEVEL": "CRITICAL",
})

# ── Заглушки внешних модулей ────────────────────────────────────────────────
_aiohttp = types.ModuleType("aiohttp")
_web = types.ModuleType("aiohttp.web")
for _name in ("Application", "Response", "Request", "AppRunner", "TCPSite"):
    setattr(_web, _name, type(_name, (), {}))
_web.json_response = lambda *a, **k: None
_aiohttp.web = _web
sys.modules.setdefault("aiohttp", _aiohttp)
sys.modules.setdefault("aiohttp.web", _web)

_pw = types.ModuleType("patchright")
_pw_api = types.ModuleType("patchright.async_api")
_pw_api.async_playwright = lambda: None
_pw.async_api = _pw_api
sys.modules.setdefault("patchright", _pw)
sys.modules.setdefault("patchright.async_api", _pw_api)

sys.path.insert(0, str(Path(__file__).resolve().parent))
import server  # noqa: E402


class HangingPage:
    """Страница, чей драйвер мёртв: любой await по ней не возвращается никогда.
    Ровно так вёл себя браузер в инциденте."""

    def __init__(self):
        self.mouse = self

    async def goto(self, *a, **k):
        await asyncio.Event().wait()

    async def move(self, *a, **k):
        await asyncio.Event().wait()

    async def wheel(self, *a, **k):
        await asyncio.Event().wait()

    async def wait_for_timeout(self, *a, **k):
        await asyncio.Event().wait()

    def on(self, *a, **k):
        pass

    def remove_listener(self, *a, **k):
        pass


def make_lane(idx=0, page=None):
    lane = server.Lane(idx, "")
    lane._page = page if page is not None else HangingPage()
    lane._browser = object()
    lane.healthy = True
    return lane


def check(cond, msg):
    print(("  OK   " if cond else "  ПРОВАЛ ") + msg)
    if not cond:
        check.failed = True


check.failed = False


async def test_fetch_deadline():
    """A. fetch_search на висящем драйвере обязан вернуться и отпустить лок."""
    lane = make_lane()
    loop = asyncio.get_event_loop()
    t0 = loop.time()
    status, _body = await asyncio.wait_for(
        lane.fetch_search("смартфон", "popular", 1), timeout=10)
    took = loop.time() - t0

    check(status in (403, 504), f"вернул код беды сайдкара, получено {status}")
    check(took < 5, f"уложился в дедлайн: {took:.2f}с")
    check(not lane.lock.locked(), "лок отпущен (старый код держал его вечно)")
    check(lane.healthy is False, "дорожка снята с healthy")


async def test_watchdog_frees_stuck_lane():
    """B. Вотчдог снимает healthy с дорожки, чей лок держат дольше порога."""
    lane = make_lane()
    pool = server.Pool([lane])

    async def hold_lock_forever():
        async with lane.lock:
            lane._lock_since = asyncio.get_event_loop().time()
            await asyncio.Event().wait()

    holder = asyncio.ensure_future(hold_lock_forever())
    await asyncio.sleep(0.05)
    # _lock_since живёт в монотонных часах сервера — выставим руками «давно».
    lane._lock_since = server.time.monotonic() - 5

    maint = asyncio.ensure_future(pool.maintenance_loop())
    await asyncio.sleep(0.6)
    maint.cancel()
    holder.cancel()

    check(lane.healthy is False, "вотчдог снял healthy с залипшей дорожки")
    check(lane._needs_relaunch, "дорожка помечена на пересоздание")
    check(pool.pick() is None, "pick() больше не отдаёт залипшую дорожку")


async def test_stuck_lane_does_not_block_others():
    """C. Зависшая дорожка не мешает обслуживать соседнюю."""
    stuck, alive = make_lane(0), make_lane(1)
    pool = server.Pool([stuck, alive])

    async def hold_lock_forever():
        async with stuck.lock:
            await asyncio.Event().wait()

    holder = asyncio.ensure_future(hold_lock_forever())
    await asyncio.sleep(0.05)
    stuck._lock_since = server.time.monotonic() - 5

    warmed = {"n": 0}

    async def fake_warm():
        warmed["n"] += 1
        alive.healthy = True
        alive._last_warm = server.time.monotonic()

    alive.warm = fake_warm
    alive.healthy = False          # просится на перепрогрев
    alive._next_warm = 0.0

    maint = asyncio.ensure_future(pool.maintenance_loop())
    await asyncio.sleep(0.8)
    maint.cancel()
    holder.cancel()

    check(warmed["n"] > 0, "соседняя дорожка обслужена, несмотря на залипшую")


async def test_last_success_age_grows():
    """D. Возраст последнего успеха растёт — на нём висит алерт."""
    server._last_success_at = server.time.monotonic() - 42
    age = server.time.monotonic() - server._last_success_at
    check(age >= 42, f"возраст последнего успеха отражает простой: {age:.0f}с")
    server._mark_success()
    age2 = server.time.monotonic() - server._last_success_at
    check(age2 < 1, "успешный запрос обнуляет возраст")


async def main():
    for t in (test_fetch_deadline, test_watchdog_frees_stuck_lane,
              test_stuck_lane_does_not_block_others, test_last_success_age_grows):
        print(f"\n{t.__doc__.splitlines()[0]}")
        await t()
    print("\nПРОВАЛЕНО" if check.failed else "\nвсё зелено")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
