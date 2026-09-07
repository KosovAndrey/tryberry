#!/usr/bin/env python3
"""
Регрессия на инцидент 07-09-2026: FAB перестал пускать анонимные дорожки с нашего
IP, выжила одна authed, и тихий фолбэк в Pool.pick/pick_any десять часов гнал через
неё ~250 запросов/час — массовый поток шёл под аккаунтом, а метрики показывали
живой пул (healthy_lanes=1) и 200-е ответы.

Свойство, которого не было у старого кода: массовый поток НИКОГДА не попадает на
authed-дорожку. Нет анонимных — отказ (502) и счётчик, а не молчаливая подмена:
потеря площадки обратима, бан аккаунта — нет. Доступ к authed остаётся ровно один
— явный, через pick_authed (18+ товары, /scrape?authed=1).

Запуск: python3 ozon-miner/test_pool_pick.py
Зависимостей нет — aiohttp/camoufox замоканы.
"""
import os
import sys
import types
from pathlib import Path

os.environ.update({
    "OZON_POOL_SIZE": "3",
    "LOG_LEVEL": "CRITICAL",
})

# ── Заглушки внешних модулей (см. test_lane_deadlock.py) ────────────────────
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

# Кука с этим ключом = залогиненная сессия (Lane.authed смотрит именно на неё).
AUTHED_COOKIE = "__Secure-access-token=xxx"


def make_lane(idx, authed=False, healthy=True):
    lane = server.Lane({"idx": idx, "proxy": "", "cookie": AUTHED_COOKIE if authed else ""})
    lane.healthy = healthy
    return lane


def check(cond, msg):
    print(("  OK   " if cond else "  ПРОВАЛ ") + msg)
    if not cond:
        check.failed = True


check.failed = False


def test_prefers_anonymous():
    """A. Пока анонимные живы, массовый поток идёт только по ним."""
    pool = server.Pool([make_lane(0, authed=True), make_lane(1), make_lane(2)])

    picked = {pool.pick(str(pid)).idx for pid in range(100, 140)}
    check(picked and 0 not in picked, f"pick() не трогает authed-дорожку: выбраны {sorted(picked)}")

    any_picked = {pool.pick_any().idx for _ in range(40)}
    check(any_picked and 0 not in any_picked,
          f"pick_any() не трогает authed-дорожку: выбраны {sorted(any_picked)}")


def test_no_fallback_to_authed():
    """B. Анонимные мертвы, жива authed — массовому потоку отказ, а не подмена."""
    authed = make_lane(0, authed=True)
    pool = server.Pool([authed, make_lane(1, healthy=False), make_lane(2, healthy=False)])
    before = server._anon_starved_total

    check(pool.pick("12345") is None, "pick() вернул None (хендлер отдаст 502)")
    check(pool.pick_any() is None, "pick_any() вернул None")
    check(server._anon_starved_total == before + 2,
          f"счётчик отказов вырос на 2: {server._anon_starved_total - before}")
    check(pool.healthy_count() == 1 and pool.anon_healthy_count() == 0,
          "пул честно показывает: живых 1, анонимных 0 (на этом висит OzonMinerNoAnonLanes)")


def test_authed_still_reachable():
    """C. 18+ по-прежнему обслуживается — явным путём pick_authed."""
    authed = make_lane(0, authed=True)
    pool = server.Pool([authed, make_lane(1, healthy=False)])

    check(pool.pick_authed() is authed, "pick_authed() отдаёт залогиненную дорожку")
    check(pool.authed_healthy_count() == 1, "authed_healthy_count() = 1")

    authed.healthy = False
    check(pool.pick_authed() is None, "мёртвая authed → None (18+ недоступны, 502)")


def test_sharding_stable():
    """D. Липкость товар→дорожка сохранилась: один id всегда на одной дорожке."""
    pool = server.Pool([make_lane(0, authed=True), make_lane(1), make_lane(2)])
    picks = {pool.pick("4141683910").idx for _ in range(20)}
    check(len(picks) == 1, f"один товар — одна дорожка: {picks}")


def main():
    for t in (test_prefers_anonymous, test_no_fallback_to_authed,
              test_authed_still_reachable, test_sharding_stable):
        print(f"\n{t.__doc__.splitlines()[0]}")
        t()
    print("\nПРОВАЛЫ ЕСТЬ" if check.failed else "\nвсе проверки прошли")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(main())
