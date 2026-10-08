#!/usr/bin/env python3
"""
Регрессия на батчер карточек: пачки должны обслуживаться ПАРАЛЛЕЛЬНО по числу
дорожек. Раньше CardBatcher.loop ждал каждую пачку целиком, и при нескольких
дорожках все, кроме одной, простаивали — очередь живых цен копилась до таймаута
Go, а товар оставался без цены.

Запуск: python3 wb-search-miner/test_card_batcher.py
Зависимостей нет — aiohttp/patchright замоканы.
"""
import asyncio
import json
import os
import sys
import time
import types
from pathlib import Path

os.environ.update({
    "WB_CARD_BATCH_WINDOW_MS": "10",
    "WB_CARD_BATCH_MAX": "1",
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

FETCH_S = 0.4


class FakeLane:
    def __init__(self, idx):
        self.idx = idx
        self.healthy = True
        self.lock = asyncio.Lock()

    async def fetch_card(self, nms):
        async with self.lock:
            await asyncio.sleep(FETCH_S)
            return 200, json.dumps({"products": [{"id": int(n)} for n in nms.split(";")]}).encode()

    async def fetch_card_detail(self, nm):
        return 404, b""


class FailingLane(FakeLane):
    async def fetch_card(self, nms):
        raise RuntimeError("драйвер умер")


async def run(lanes, n):
    pool = server.Pool(lanes)
    b = server.CardBatcher(pool)
    loop_task = asyncio.create_task(b.loop())
    t0 = time.monotonic()
    res = await asyncio.wait_for(
        asyncio.gather(*(b.get(str(100 + i)) for i in range(n))), timeout=10)
    loop_task.cancel()
    return time.monotonic() - t0, res


async def main():
    ok = True

    # 2 дорожки, 2 пачки по одному артикулу: параллельно ≈ 1×FETCH_S, а не 2×.
    took, res = await run([FakeLane(0), FakeLane(1)], 2)
    good = took < 1.6 * FETCH_S and all(st == 200 and p for st, p in res)
    print(("  OK  " if good else "  FAIL"), "2 пачки на 2 дорожках за %.2fс (последовательно было бы %.2fс)" % (took, 2 * FETCH_S))
    ok &= good

    # 1 дорожка: параллельность не обгоняет лок — wbaas не получает лишнего.
    took, res = await run([FakeLane(0)], 2)
    good = took >= 1.9 * FETCH_S and all(st == 200 for st, _ in res)
    print(("  OK  " if good else "  FAIL"), "1 дорожка держит последовательность: %.2fс" % took)
    ok &= good

    # Упавшая пачка не вешает ждущих: все получают 502, а не таймаут.
    took, res = await run([FailingLane(0)], 2)
    good = all(st == 502 for st, _ in res)
    print(("  OK  " if good else "  FAIL"), "падение пачки → 502 всем ждущим")
    ok &= good

    print("\nвсё зелено" if ok else "\nЕСТЬ ПАДЕНИЯ")
    return ok


if __name__ == "__main__":
    sys.exit(0 if asyncio.run(main()) else 1)
