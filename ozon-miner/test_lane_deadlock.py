#!/usr/bin/env python3
"""
Регрессия на инцидент 29-07-2026: подвисший драйвер camoufox держал лок дорожки
вечно (пауза между ретраями шла через page.wait_for_timeout), обслуживающий цикл
пропускал залоченные дорожки, и пул 11 часов отдавал ошибки при healthy_lanes=4.

Тест не поднимает браузер: подменяет страницу заглушкой, которая ВИСНЕТ. Проверяем
свойства, которых у старого кода не было:
  A. fetch_path на висящем драйвере возвращается по дедлайну (504), лок отпущен;
  B. вотчдог снимает healthy с дорожки, чей лок держат дольше порога;
  C. зависшее обслуживание одной дорожки не мешает обслуживать остальные;
  D. возраст последнего успеха растёт (метрика, на которой висит алерт);
  E. зажившийся браузер планово пересоздаётся — инцидент 02-09-2026: за 5 суток
     аптайма camoufox перестаёт проходить FAB, пул тихо деградировал до одной
     дорожки из шести и p95 скрейпа уехал на 26-29с.
  F. дорожка, которая не может прогреться, получает НОВЫЙ браузер — инцидент
     08-09-2026: recycle из E берёт только здоровые дорожки, а warm() браузер не
     пересоздаёт; анонимные дорожки шесть часов перепрогревались на том же
     зажившемся профиле (35 попыток, один успех) и поднялись только руками.

Запуск: python3 ozon-miner/test_lane_deadlock.py
Зависимостей нет — aiohttp/camoufox замоканы (в контейнере они есть, но тесту
они не нужны, а гонять его хочется где угодно).
"""
import asyncio
import os
import sys
import types
from pathlib import Path

# ── Пороги делаем маленькими ДО импорта server.py (константы читаются на импорте)
os.environ.update({
    "OZON_POOL_SIZE": "2",
    "OZON_SCRAPE_TIMEOUT_SECONDS": "0.3",
    "OZON_SCRAPE_RETRIES": "2",
    "OZON_RETRY_PAUSE_MS": "50",
    "OZON_LANE_OP_TIMEOUT_SECONDS": "1",
    "OZON_MAINT_OP_TIMEOUT_SECONDS": "1",
    "OZON_LANE_STUCK_SECONDS": "1",
    "OZON_DRIVER_CALL_TIMEOUT_SECONDS": "1",
    "OZON_HEALTH_INTERVAL_SECONDS": "0.2",
    "OZON_WARM_RELAUNCH_AFTER_FAILS": "3",
    "OZON_LANE_MIN_INTERVAL_MS": "0",
    "LOG_LEVEL": "CRITICAL",
})

# ── Заглушки внешних модулей ────────────────────────────────────────────────
_aiohttp = types.ModuleType("aiohttp")
_web = types.ModuleType("aiohttp.web")
for _name in ("Application", "Response", "Request", "AppRunner", "TCPSite"):
    setattr(_web, _name, type(_name, (), {}))
_web.json_response = lambda *a, **k: None
_aiohttp.web = _web
_aiohttp.ClientSession = object
_aiohttp.ClientTimeout = lambda **k: None
sys.modules.setdefault("aiohttp", _aiohttp)
sys.modules.setdefault("aiohttp.web", _web)

_cam = types.ModuleType("camoufox")
_cam_api = types.ModuleType("camoufox.async_api")
_cam_api.AsyncCamoufox = type("AsyncCamoufox", (), {})
_cam.async_api = _cam_api
sys.modules.setdefault("camoufox", _cam)
sys.modules.setdefault("camoufox.async_api", _cam_api)

sys.path.insert(0, str(Path(__file__).resolve().parent))
import server  # noqa: E402


class HangingPage:
    """Страница, чей драйвер мёртв: любой await по ней не возвращается никогда.
    Ровно так вёл себя camoufox в инциденте."""

    def __init__(self):
        self.mouse = self

    async def evaluate(self, *a, **k):
        await asyncio.Event().wait()

    async def wheel(self, *a, **k):
        await asyncio.Event().wait()

    async def wait_for_timeout(self, *a, **k):
        await asyncio.Event().wait()

    async def goto(self, *a, **k):
        await asyncio.Event().wait()


def make_lane(idx=0, page=None):
    lane = server.Lane({"idx": idx, "proxy": "", "cookie": ""})
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
    """A. fetch_path на висящем драйвере обязан вернуться по дедлайну и отпустить лок."""
    lane = make_lane()
    t0 = asyncio.get_event_loop().time()
    status, body = await asyncio.wait_for(lane.fetch_path("/product/1/", "test"), timeout=10)
    took = asyncio.get_event_loop().time() - t0

    check(status == 504, f"вернул 504 (Go трактует как беду сайдкара), получено {status}")
    check(took < 5, f"уложился в дедлайн: {took:.2f}с")
    check(not lane.lock.locked(), "лок отпущен (старый код держал его вечно)")
    check(lane.healthy is False, "дорожка снята с healthy")
    check(lane._needs_relaunch, "помечена на пересоздание браузера")


async def test_watchdog_frees_stuck_lane():
    """B. Вотчдог снимает healthy с дорожки, чей лок держат дольше порога."""
    lane = make_lane()
    pool = server.Pool([lane])

    async def hold_lock_forever():
        async with lane.lock:
            lane._lock_since = asyncio.get_event_loop().time()
            lane._lock_since = server.time.monotonic()
            await asyncio.Event().wait()

    holder = asyncio.ensure_future(hold_lock_forever())
    await asyncio.sleep(0.1)
    check(pool.pick("1") is lane, "пока не залипла — пул её выдаёт")

    loop_task = asyncio.ensure_future(pool.maintenance_loop())
    await asyncio.sleep(server.LANE_STUCK_S + 0.6)

    check(lane.healthy is False, "вотчдог снял healthy с залипшей дорожки")
    check(pool.pick("1") is None, "пул больше не отдаёт залипшую дорожку")
    check(pool.stuck_lanes(server.time.monotonic()) == 1, "метрика stuck_lanes видит залипание")

    loop_task.cancel()
    holder.cancel()


async def test_stuck_lane_does_not_freeze_pool():
    """C. Зависшее обслуживание одной дорожки не мешает обслуживать остальные.
    Это и есть суть инцидента: цикл шёл последовательно и замирал на первой же."""
    stuck = make_lane(0)
    healthy = make_lane(1, page=HangingPage())
    stuck.healthy = False          # просится на перепрогрев и там зависнет
    healthy.healthy = False
    pool = server.Pool([stuck, healthy])

    serviced = []
    orig_warm = server.Lane.warm

    async def fake_warm(self):
        serviced.append(self.idx)
        if self.idx == 0:
            await asyncio.Event().wait()   # дорожка 0 виснет намертво
        self.healthy = True

    server.Lane.warm = fake_warm
    try:
        loop_task = asyncio.ensure_future(pool.maintenance_loop())
        await asyncio.sleep(0.8)
        loop_task.cancel()
    finally:
        server.Lane.warm = orig_warm

    check(0 in serviced, "дорожка 0 (зависшая) обслуживалась")
    check(1 in serviced, "дорожка 1 обслужена НЕСМОТРЯ на зависшую соседку")
    check(healthy.healthy is True, "дорожка 1 успешно прогрелась")


async def test_planned_recycle():
    """E. Зажившийся браузер планово пересоздаётся (инцидент 02-09-2026)."""
    lane = make_lane()
    now = server.time.monotonic()
    lane._launched_at = now
    lane._age_limit = 0.3

    # ── Чистая логика, без сна: именно она задаёт поведение, и проверять её
    # таймингами нельзя (первая версия теста ловила «ровно один recycle» за
    # окно, в которое дорожка успевала состариться дважды, и проходила лишь
    # по удачному совпадению).
    check(not lane.due_recycle(now), "молодой браузер не трогаем")
    check(lane.due_recycle(now + 0.4), "зажившийся просится на recycle")

    lane._launched_at = now + 0.4          # так делает _launch() после пересоздания
    check(not lane.due_recycle(now + 0.5), "после пересоздания снова молодой — цикла нет")

    lane.healthy = False
    check(not lane.due_recycle(now + 10), "нездоровую дорожку recycle не перехватывает")
    lane.healthy = True

    saved, server.LANE_MAX_AGE_S = server.LANE_MAX_AGE_S, 0
    check(not lane.due_recycle(now + 10), "нулём recycle выключается")
    server.LANE_MAX_AGE_S = saved

    # ── И через обслуживающий цикл: дорожка реально уходит на пересоздание,
    # прогревается и остаётся здоровой (сколько раз за окно — не наше дело).
    lane._launched_at = server.time.monotonic() - 10
    lane._next_warm = server.time.monotonic() + 9999   # backoff от прошлых неудач
    pool = server.Pool([lane])

    warmed = []
    orig_warm = server.Lane.warm

    async def fake_warm(self):
        warmed.append(self.idx)
        self._needs_relaunch = False
        self._launched_at = server.time.monotonic()
        self.healthy = True

    server.Lane.warm = fake_warm
    try:
        loop_task = asyncio.ensure_future(pool.maintenance_loop())
        await asyncio.sleep(0.5)
        loop_task.cancel()
    finally:
        server.Lane.warm = orig_warm

    check(len(warmed) >= 1, f"дорожка ушла на recycle и прогрелась заново: {warmed}")
    check(lane.healthy and not lane._needs_relaunch,
          "после recycle дорожка снова здорова и не висит в ожидании")


async def test_relaunch_after_failed_warms():
    """F. Дорожка, которая не может прогреться, обязана получить НОВЫЙ браузер."""
    lane = make_lane()
    lane._launched_at = server.time.monotonic() - 40000   # зажившийся профиль

    lane._warm_backoff("прогрев не дал 200")
    check(not lane._needs_relaunch, "после первой неудачи браузер не трогаем")
    lane._warm_backoff("прогрев не дал 200")
    check(not lane._needs_relaunch, "после второй — тоже (транзиент)")
    lane._warm_backoff("прогрев не дал 200")
    check(lane._needs_relaunch,
          f"после {server.WARM_RELAUNCH_AFTER_FAILS}-й неудачи подряд — новый браузер")

    # Успешный прогрев обнуляет счётчик: следующая серия считается заново, а не
    # добивает пересоздание с первой же неудачи.
    lane._needs_relaunch = False
    lane._warm_fails = 0
    lane._warm_backoff("прогрев не дал 200")
    check(not lane._needs_relaunch, "счётчик обнулён успехом — отсчёт заново")

    # Рубильник: 0 выключает пересоздание целиком.
    saved, server.WARM_RELAUNCH_AFTER_FAILS = server.WARM_RELAUNCH_AFTER_FAILS, 0
    try:
        lane._warm_fails = 0
        lane._needs_relaunch = False
        for _ in range(10):
            lane._warm_backoff("прогрев не дал 200")
        check(not lane._needs_relaunch, "нулём пересоздание выключается")
    finally:
        server.WARM_RELAUNCH_AFTER_FAILS = saved

    # И через warm(): _needs_relaunch обязан привести к _relaunch(), а не к
    # ре-навигации по старой странице (ровно этого не хватало 08-09).
    lane._warm_fails = 0
    lane._needs_relaunch = True
    relaunched = []

    async def fake_relaunch(self):
        relaunched.append(self.idx)
        self._needs_relaunch = False
        return False          # браузер не поднялся → warm() выходит на backoff

    orig = server.Lane._relaunch
    server.Lane._relaunch = fake_relaunch
    try:
        await lane.warm()
    finally:
        server.Lane._relaunch = orig
    check(relaunched == [lane.idx], f"warm() пересоздал браузер: {relaunched}")


async def test_last_success_age():
    """D. Метрика возраста последнего успеха — на ней висит алерт OzonMinerStale."""
    server._mark_success()
    await asyncio.sleep(0.2)
    age = server.time.monotonic() - server._last_success_at
    check(0.15 < age < 2, f"возраст последнего успеха растёт: {age:.2f}с")


async def main():
    for t in (test_fetch_deadline, test_watchdog_frees_stuck_lane,
              test_stuck_lane_does_not_freeze_pool, test_planned_recycle,
              test_relaunch_after_failed_warms, test_last_success_age):
        print(f"\n{t.__doc__.splitlines()[0]}")
        await t()
    print("\nПРОВАЛЫ ЕСТЬ" if check.failed else "\nвсе проверки прошли")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
