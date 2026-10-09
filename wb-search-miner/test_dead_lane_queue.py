#!/usr/bin/env python3
"""
Регрессия: запрос, ждавший лок дорожки, НЕ идёт в WB, если она выпала, пока он
ждал (соседний запрос поймал 498). Прод 09-10-2026: ~треть всех 498 — такие
хвосты очереди, пары 498 на одной дорожке через 0.1–1с; каждый — гарантированно
сгоревший запрос карточки и лок, держащий перепрогрев.

Запуск: python3 wb-search-miner/test_dead_lane_queue.py
Зависимостей нет — aiohttp/patchright замоканы.
"""
import asyncio
import os
import sys
import types
from pathlib import Path

os.environ.update({
    "WB_LANE_MIN_INTERVAL_MS": "1",
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


class FakeLane(server.Lane):
    """Настоящая Lane, но in-page fetch — скрипт ответов без браузера."""

    def __init__(self, idx, answers):
        super().__init__(idx, "")
        self.answers = list(answers)
        self.sent = 0

    async def _api_fetch(self, rel_url, what):
        self.sent += 1
        await asyncio.sleep(0.05)  # запрос в полёте — лок занят
        status = self.answers.pop(0)
        if status != 200:
            self.healthy = False  # как в настоящем _api_fetch на 498
        return status, b'{"products": []}'


async def main():
    # Три запроса встали в очередь к одной дорожке; первый ловит 498.
    lane = FakeLane(0, [498, 200, 200])
    lane.healthy = True
    res = await asyncio.gather(*(lane.fetch_card(str(i)) for i in range(3)))
    check(lane.sent == 1, f"в WB ушёл только первый запрос (ушло: {lane.sent})")
    check([r[0] for r in res] == [498, 0, 0],
          f"хвост очереди получил 0 = «дорожка не ответила» ({[r[0] for r in res]})")

    lane = FakeLane(1, [498, 200])
    lane.healthy = True
    res = await asyncio.gather(lane.fetch_card("1"), lane.fetch_card_detail("2"))
    check(lane.sent == 1 and res[1][0] == 0, "detail в хвосте тоже не шлётся")

    # Код 0 у батчера = неудачная попытка; вторую pick() отдаст живой соседке.
    dead = FakeLane(2, [])
    alive = FakeLane(3, [200])
    alive.healthy = True
    pool = server.Pool([dead, alive])
    check(pool.pick() is alive, "pick не выдаёт выпавшую дорожку")

    # Здоровая дорожка работает как раньше.
    ok = FakeLane(4, [200, 200])
    ok.healthy = True
    res = await asyncio.gather(ok.fetch_card("1"), ok.fetch_card("2"))
    check(ok.sent == 2 and [r[0] for r in res] == [200, 200], "здоровая очередь не затронута")


asyncio.run(main())
sys.exit(1 if failed else 0)
