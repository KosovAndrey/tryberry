#!/usr/bin/env python3
"""
Регрессия: выпавшая из healthy дорожка уходит в перепрогрев СРАЗУ, а не на
следующем тике обслуживания (MAINT_INTERVAL_S, в проде 30с). Замер 08-10-2026:
~40 выпадений по 498 в час на три дорожки и 9–15% времени без единой живой —
львиную долю окна давало ожидание тика, а не сам прогрев.

Запуск: python3 wb-search-miner/test_rewarm_kick.py
Зависимостей нет — aiohttp/patchright замоканы.
"""
import asyncio
import os
import sys
import types
from pathlib import Path

os.environ.update({
    "WB_HEALTH_INTERVAL_SECONDS": "30",  # как в проде: тик сам не придёт
    "LOG_LEVEL": "CRITICAL",
})

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

failed = False


def check(cond, msg):
    global failed
    print(("  OK   " if cond else "  ПРОВАЛ ") + msg)
    failed |= not cond


class CountingLane(server.Lane):
    """Настоящая Lane, но прогрев — счётчик без браузера."""

    def __init__(self, idx):
        super().__init__(idx, "")
        self.warms = 0

    async def warm(self):
        self.warms += 1
        self.healthy = True


async def main():
    lane, other = CountingLane(0), CountingLane(1)
    lane.healthy = other.healthy = True
    pool = server.Pool([lane, other])
    maint = asyncio.ensure_future(pool.maintenance_loop())
    await asyncio.sleep(0.05)

    lane.healthy = False  # 498: дорожка выпала
    await asyncio.sleep(0.5)
    check(lane.warms == 1, f"перепрогрев начался сразу, не через 30с (прогревов: {lane.warms})")
    check(lane.healthy, "дорожка снова в healthy")
    check(other.warms == 0, "здоровую соседку не трогали")

    # Неудачный прогрев: backoff уважается — внеочередной тик не долбит повторно.
    async def failing_warm():
        lane.warms += 1
        lane.healthy = False
        lane._next_warm = server.time.monotonic() + 60
    lane.warm = failing_warm
    lane.warms = 0
    lane.healthy = False
    await asyncio.sleep(0.5)
    check(lane.warms == 1, f"после неудачи ждём backoff, без долбёжки (прогревов: {lane.warms})")

    # Выпала ПОД ЛОКОМ (498 на поиске, дальше стоянка под тем же локом):
    # внеочередной тик застаёт лок занятым — перепрогрев обязан начаться вскоре
    # после освобождения, а не через MAINT_INTERVAL_S.
    busy = CountingLane(2)
    busy.healthy = True
    pool2 = server.Pool([busy])
    maint2 = asyncio.ensure_future(pool2.maintenance_loop())
    await asyncio.sleep(0.05)
    async with busy.lock:
        busy.healthy = False
        await asyncio.sleep(0.3)  # «уход на стоянку»
    await asyncio.sleep(1.5)
    check(busy.warms == 1, f"выпавшая под локом прогрета сразу после освобождения (прогревов: {busy.warms})")

    maint.cancel()
    maint2.cancel()


asyncio.run(main())
sys.exit(1 if failed else 0)
