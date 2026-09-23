#!/usr/bin/env python3
"""
Регрессия на путь к __internal (23-09-2026).

В тот день WB закрыл 403-м ВСЕ публичные хосты (card/u-card/search/u-search) для
всех, включая домашний RU-IP, и оставил данные только за фронтом. Ключ к ним —
заголовок `deviceid`: без него даже прогретая страница получает 403, с ним 200.
Забыть этот заголовок = тихо потерять и цены, и поиск, поэтому он под тестом.

Проверяем:
  A. in-page fetch уходит с заголовком deviceid вида site_<32 hex>;
  B. GET /card ходит к u-card пачкой nm=id1;id2 и отдаёт тело как есть;
  C. поиск идёт дешёвым in-page fetch'ем, а пагинация попадает в URL
     (через навигацию &page=N не работал — WB всегда отдавал первую страницу);
  D. 403 от __internal снимает healthy — дорожку надо прогревать заново;
  E. у каждой дорожки свой deviceid, и пересоздание браузера его меняет.

Запуск: python3 wb-search-miner/test_internal_fetch.py
Зависимостей нет — aiohttp/patchright замоканы.
"""
import asyncio
import os
import re
import sys
import types
from pathlib import Path

os.environ.update({
    "WB_SEARCH_POOL_SIZE": "1",
    "WB_FETCH_TIMEOUT_SECONDS": "0.5",
    "WB_LANE_MIN_INTERVAL_MS": "0",
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


class FakePage:
    """Страница, у которой evaluate возвращает заранее заданный ответ и
    запоминает, с чем его позвали."""

    def __init__(self, status=200, body='{"products":[]}'):
        self.status = status
        self.body = body
        self.calls = []

    async def evaluate(self, js, arg):
        self.calls.append(arg)
        return {"s": self.status, "b": self.body}


def make_lane(page):
    lane = server.Lane(0, "")
    lane._page = page
    lane._browser = object()
    lane.healthy = True
    return lane


def check(cond, msg):
    print(("  OK   " if cond else "  ПРОВАЛ ") + msg)
    if not cond:
        check.failed = True


check.failed = False


async def main():
    print("A. deviceid уходит в каждый запрос — без него __internal даёт 403.")
    page = FakePage(body='{"products":[{"id":1}]}')
    lane = make_lane(page)
    status, body = await lane.fetch_card("211695539")
    url, dev = page.calls[-1]
    check(status == 200 and b'"id":1' in body, "тело карточки отдано как есть")
    check(re.fullmatch(r"site_[0-9a-f]{32}", dev) is not None,
          "deviceid имеет вид site_<32 hex>: %s" % dev)

    print("\nB. /card ходит к u-card пачкой артикулов.")
    lane2 = make_lane(FakePage())
    await lane2.fetch_card("111;222")
    url2, _ = lane2._page.calls[-1]
    check(server.UCARD_PATH in url2, "путь u-card: %s" % url2.split("?")[0])
    check("nm=111%3B222" in url2 or "nm=111;222" in url2,
          "оба артикула в одном запросе")

    print("\nC. Поиск идёт in-page fetch'ем, страница попадает в URL.")
    lane3 = make_lane(FakePage())
    await lane3.fetch_search("rtx 5080", "popular", 3)
    url3, _ = lane3._page.calls[-1]
    check(server.USEARCH_PATH in url3, "путь u-search без навигации")
    check("page=3" in url3, "пагинация в URL: %s" % url3[-40:])
    check("query=rtx%205080" in url3 or "query=rtx+5080" in url3, "запрос закодирован")

    print("\nD. 403 от __internal снимает healthy — нужен новый прогрев.")
    lane4 = make_lane(FakePage(status=403, body=""))
    status4, _ = await lane4.fetch_card("211695539")
    check(status4 == 403, "статус зеркалится наверх")
    check(lane4.healthy is False, "дорожка помечена нездоровой")

    print("\nE. deviceid свой у дорожки и меняется при пересоздании браузера.")
    a, b = server.Lane(0, ""), server.Lane(1, "")
    check(a.device_id != b.device_id, "у разных дорожек разные deviceid")
    before = a.device_id
    a._page = FakePage()
    a._browser = None
    a._pw = None

    async def _boom():
        raise RuntimeError("launch отключён в тесте")

    a._launch = _boom
    a.close = lambda: asyncio.sleep(0)
    await a._relaunch()
    check(a.device_id != before, "пересоздание выдало новый deviceid")

    print("\nвсё зелено" if not check.failed else "\nЕСТЬ ПРОВАЛЫ")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
