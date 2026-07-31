#!/usr/bin/env python3
"""
Регрессия на утечку деревьев процессов camoufox (31-07-2026).

Про цифры: поводом была версия «1263 процесса за 11 часов», но она оказалась
ложной — колонка PIDs в docker stats считает ПОТОКИ, и ~1200 потоков при ~52
процессах у шести дорожек это норма. А вот три дыры ниже настоящие: каждая
оставляла жить дерево node+firefox при пересоздании дорожки.

Три дыры, каждая проверяется отдельно:
  A. фолбэк geoip создавал ВТОРУЮ инстанцию camoufox, потеряв первую без
     __aexit__ — её node-драйвер и firefox оставались жить;
  B. close() при подвисшем драйвере отваливался по таймауту __aexit__ и дерево
     процессов не убиралось вовсе;
  C. осиротевшие деревья (родитель умер, усыновил PID 1) не подбирал никто.

Плюс D: разбор /proc, на котором держится и уборщик, и метрики.

Запуск: python3 ozon-miner/test_process_leak.py
Зависимостей нет — aiohttp/camoufox замоканы, процессы настоящие (sleep).
"""
import asyncio
import os
import subprocess
import sys
import types
from pathlib import Path

os.environ.update({
    "OZON_POOL_SIZE": "1",
    "OZON_DRIVER_CALL_TIMEOUT_SECONDS": "0.3",
    "OZON_KILL_GRACE_SECONDS": "0.3",
    "OZON_ORPHAN_MIN_AGE_SECONDS": "0",
    "LOG_LEVEL": "CRITICAL",
})

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


def check(cond, msg):
    print(("  OK   " if cond else "  ПРОВАЛ ") + msg)
    if not cond:
        check.failed = True


check.failed = False


def spawn_sleeper(seconds=60):
    """Настоящий дочерний процесс — заменяет node-драйвер playwright."""
    return subprocess.Popen([sys.executable, "-c", f"import time; time.sleep({seconds})"])


def alive(pid: int) -> bool:
    """Живость с учётом зомби: убитый ребёнок, которого никто не сжал wait'ом,
    остаётся в таблице процессов, и os.kill(pid, 0) на нём проходит успешно —
    первая версия теста из-за этого ложно ругалась на рабочий фикс."""
    try:
        with open(f"/proc/{pid}/stat") as f:
            raw = f.read()
        state = raw[raw.rindex(")") + 2:].split()[0]
    except (OSError, ValueError, IndexError):
        return False
    return state != "Z"


class FakeCam:
    """Инстанция camoufox: __aenter__ либо поднимает 'драйвер' (реальный дочерний
    процесс), либо падает — как настоящая при недоступном geoip."""

    def __init__(self, *, fail=False, hang_close=False):
        self.fail = fail
        self.hang_close = hang_close
        self.proc = None
        self.exited = False

    async def __aenter__(self):
        self.proc = spawn_sleeper()
        await asyncio.sleep(0.05)
        if self.fail:
            raise RuntimeError("geoip database not found")
        return object()

    async def __aexit__(self, *exc):
        self.exited = True
        if self.hang_close:
            await asyncio.Event().wait()   # подвисший драйвер: закрытие не возвращается
        self.proc.terminate()


async def test_geoip_fallback_closes_first():
    """A. Фолбэк geoip обязан закрыть первую инстанцию, а не бросить её жить."""
    made = []

    def factory(**kw):
        cam = FakeCam(fail=("geoip" in kw))
        made.append(cam)
        return cam

    server.AsyncCamoufox = factory
    lane = server.Lane({"idx": 0, "proxy": "", "cookie": ""})
    lane._browser = None

    async def fake_new_page(*a, **k):
        raise RuntimeError("страница не нужна")

    try:
        await lane._launch()
    except Exception:  # noqa: BLE001 — падение на new_page нам не мешает
        pass

    check(len(made) == 2, f"попыток запуска две (с geoip и без), получено {len(made)}")
    check(made[0].exited, "первая (упавшая) инстанция закрыта — раньше терялась молча")
    await asyncio.sleep(0.4)
    check(not alive(made[0].proc.pid), "её процесс-драйвер не остался жить")
    check(lane._proc_pids, "PID'ы запуска записаны — по ним потом добиваем дерево")

    for c in made:
        if c.proc:
            c.proc.kill()


async def test_close_kills_hung_driver():
    """B. Подвисшее закрытие не должно оставлять дерево процессов."""
    cam = FakeCam(hang_close=True)
    server.AsyncCamoufox = lambda **kw: cam
    lane = server.Lane({"idx": 0, "proxy": "", "cookie": ""})
    lane._cam = cam
    proc = spawn_sleeper()
    lane._proc_pids = {proc.pid}

    await lane.close()

    check(not alive(proc.pid),
          "процесс убит, хотя __aexit__ завис (раньше он жил вечно)")
    check(not lane._proc_pids, "список PID'ов очищен после закрытия")
    if cam.proc:
        cam.proc.kill()


async def test_reaper_selects_only_orphan_browsers():
    """C. Уборщик берёт ТОЛЬКО осиротевшие браузеры и ничего больше.

    Снимок процессов здесь целиком синтетический, и убийство подменено счётчиком:
    первая версия теста звала настоящий reap_orphans() на живой машине, где у
    системных служб ppid==1 — так можно снести чужое. Предохранители в коде
    (только внутри контейнера, только имена браузеров) проверяем на этих данных."""
    mine = os.getpid()
    fake = {
        mine: {"ppid": 1, "comm": "python3", "rss": 0, "started": 0.0},
        101: {"ppid": 1, "comm": "node", "rss": 0, "started": 0.0},        # сирота-драйвер
        102: {"ppid": 101, "comm": "firefox", "rss": 0, "started": 0.0},   # его потомок
        103: {"ppid": 1, "comm": "systemd", "rss": 0, "started": 0.0},     # чужое, не трогать
        104: {"ppid": mine, "comm": "firefox", "rss": 0, "started": 0.0},  # наш живой браузер
    }
    killed_roots = []

    async def fake_kill(roots, what, idx=-1):
        killed_roots.extend(roots)
        return len(roots)

    real_snapshot, real_kill, real_container = (
        server._snapshot_procs, server._kill_tree, server._IN_CONTAINER)
    server._snapshot_procs = lambda: fake
    server._kill_tree = fake_kill
    try:
        server._IN_CONTAINER = False
        check(await server.reap_orphans() == 0, "вне контейнера не трогает НИЧЕГО")

        server._IN_CONTAINER = True
        killed_roots.clear()
        await server.reap_orphans()
    finally:
        server._snapshot_procs, server._kill_tree, server._IN_CONTAINER = (
            real_snapshot, real_kill, real_container)

    check(killed_roots == [101], f"выбран ровно осиротевший драйвер, взято {killed_roots}")
    check(103 not in killed_roots, "системный процесс с ppid==1 не тронут")
    check(104 not in killed_roots, "свой живой браузер не тронут")
    check(server._orphans_killed >= 1, "счётчик метрики вырос")


async def test_proc_parsing():
    """D. Разбор /proc: на нём держатся и уборщик, и метрики утечки."""
    proc = spawn_sleeper()
    await asyncio.sleep(0.1)
    procs = server._snapshot_procs()
    check(len(procs) > 1, f"снимок процессов непустой ({len(procs)})")
    check(proc.pid in procs, "новый процесс попал в снимок")
    check(proc.pid in server._own_children(procs), "он опознан как наш ребёнок")
    tree = server._tree_of(os.getpid(), procs)
    check(tree[0] == os.getpid() and proc.pid in tree,
          "дерево от нашего pid содержит потомка (по нему бьём kill)")
    n, threads, mem = server._process_stats()
    check(n > 1 and mem > 0, f"метрики считаются: процессов {n}, память {mem // 1048576}МБ")
    check(threads >= n, f"потоков не меньше процессов ({threads} против {n}) — "
                        "именно потоки показывает docker stats в колонке PIDs")
    proc.kill()


async def main():
    for t in (test_geoip_fallback_closes_first, test_close_kills_hung_driver,
              test_reaper_selects_only_orphan_browsers, test_proc_parsing):
        print(f"\n{t.__doc__.splitlines()[0]}")
        await t()
    print("\nПРОВАЛЫ ЕСТЬ" if check.failed else "\nвсе проверки прошли")
    return 1 if check.failed else 0


if __name__ == "__main__":
    sys.exit(asyncio.run(main()))
