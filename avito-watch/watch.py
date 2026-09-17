#!/usr/bin/env python3
"""
watch.py — avito-watch: личный мониторинг новых объявлений Авито в Telegram.

Кидаешь боту ссылку на выдачу с фильтрами (например, аренда квартир), он раз в
AVITO_INTERVAL_MIN..MAX секунд открывает её в браузере и присылает каждое новое
объявление: фото, цена, адрес/метро, продавец, ссылка. Для уже виденных
объявлений сообщает о снижении цены.

Почему браузер, а не запросы: 17-09-2026 домашний IP после двух голых curl
получил 429 «Доступ ограничен: проблема с IP», мобильное API — тот же файрвол.
Headful Chromium (patchright в Xvfb) — тот же движок, что проходит антиботы в
ali-miner и wb-search-miner.

Браузер НЕ живёт постоянно: запускается на время проверки и закрывается. Хост
по памяти впритык (сумма mem_limit сайдкаров уже выше физической), а раз в
несколько минут поднять Chromium — пара секунд. Cookie доверенной сессии
переживают перезапуск в постоянном профиле AVITO_DATA_DIR/profile.

Состояние (подписки, виденные объявления) — SQLite в AVITO_DATA_DIR.
Telegram — через HTTP-прокси AVITO_TG_PROXY_URL (на проде xray, прямой доступ к
api.telegram.org с VPS закрыт).
"""

import asyncio
import html
import json
import logging
import os
import random
import shutil
import sqlite3
import time
from pathlib import Path

import aiohttp
from patchright.async_api import async_playwright

import extract

# ── Конфиг ───────────────────────────────────────────────────────────────────
BOT_TOKEN = os.getenv("AVITO_BOT_TOKEN", "").strip()
ALLOWED = {int(x) for x in os.getenv("AVITO_ALLOWED_CHAT_IDS", "").replace(" ", "").split(",") if x}
TG_PROXY = os.getenv("AVITO_TG_PROXY_URL", "").strip() or None
BROWSER_PROXY = os.getenv("AVITO_BROWSER_PROXY_URL", "").strip()
DATA_DIR = Path(os.getenv("AVITO_DATA_DIR", "/data"))
INTERVAL_MIN_S = float(os.getenv("AVITO_INTERVAL_MIN_SECONDS", "180"))
INTERVAL_MAX_S = float(os.getenv("AVITO_INTERVAL_MAX_SECONDS", "300"))
# Пауза между поисками внутри одного запуска браузера.
BETWEEN_SEARCHES_S = (5.0, 15.0)
# Объявления старше этого при первом появлении не шлём: это не новое, а
# продвинутое или сдвинувшееся в выдаче.
FRESH_WINDOW_S = float(os.getenv("AVITO_FRESH_WINDOW_HOURS", "24")) * 3600
MAX_PER_CHECK = int(os.getenv("AVITO_MAX_PER_CHECK", "10"))
BLOCK_BACKOFF_MIN_S = 300.0
BLOCK_BACKOFF_MAX_S = 7200.0
# Столько блоков подряд — стираем профиль: доверие сессии, похоже, сгорело.
WIPE_PROFILE_AFTER = int(os.getenv("AVITO_WIPE_PROFILE_AFTER", "2"))
NAV_TIMEOUT_S = 60.0
CHECK_TIMEOUT_S = 120.0
HEADLESS = os.getenv("HEADLESS", "false").lower() in ("1", "true", "yes")
LOG_LEVEL = os.getenv("LOG_LEVEL", "INFO").upper()

logging.basicConfig(level=getattr(logging, LOG_LEVEL, logging.INFO),
                    format="%(asctime)s %(levelname)s %(message)s")
log = logging.getLogger("avito-watch")

HELP = (
    "Пришли ссылку на выдачу Авито с фильтрами — буду присылать новые объявления.\n\n"
    "/list — подписки\n"
    "/del N — удалить подписку N\n"
    "/check — проверить всё сейчас\n"
    "/status — состояние проверок\n"
    "/dump N — сырые данные первого объявления (для отладки)"
)

# Сбор карточек из DOM — запасной путь, если стейта на странице не нашлось.
DOM_JS = """() => Array.from(document.querySelectorAll('[data-marker="item"]')).map(el => {
  const q = s => el.querySelector(s);
  const a = q('a[itemprop="url"]') || q('a[data-marker="item-title"]');
  const img = q('img');
  return {
    id: el.getAttribute('data-item-id'),
    url: a ? a.getAttribute('href') : null,
    title: (q('[itemprop="name"]') || a || {}).textContent || '',
    price: (q('meta[itemprop="price"]') || {}).content || null,
    date: (q('[data-marker="item-date"]') || {}).textContent || '',
    address: (q('[data-marker="item-address"]') || {}).textContent || '',
    photo: img ? (img.getAttribute('src') || null) : null,
    description: (q('[class*="item-description"]') || {}).textContent || '',
    seller: Array.from(el.querySelectorAll('[class*="iva-item-user"], [data-marker*="seller"]'))
      .map(n => n.textContent.trim()).slice(0, 4),
  };
})"""
STATE_JS = """() => Array.from(document.querySelectorAll('script[type="mime/invalid"][data-mfe-state]'))
  .map(s => s.textContent)"""


# ── Хранилище ────────────────────────────────────────────────────────────────
class Store:
    def __init__(self, path: Path):
        self.db = sqlite3.connect(path)
        self.db.row_factory = sqlite3.Row
        self.db.executescript("""
            CREATE TABLE IF NOT EXISTS searches (
                id INTEGER PRIMARY KEY AUTOINCREMENT,
                chat_id INTEGER NOT NULL,
                url TEXT NOT NULL,
                title TEXT NOT NULL DEFAULT '',
                created_at REAL NOT NULL,
                primed INTEGER NOT NULL DEFAULT 0,
                next_check_at REAL NOT NULL DEFAULT 0,
                last_ok_at REAL,
                last_error TEXT,
                last_count INTEGER,
                UNIQUE (chat_id, url)
            );
            CREATE TABLE IF NOT EXISTS seen (
                search_id INTEGER NOT NULL REFERENCES searches(id) ON DELETE CASCADE,
                item_id TEXT NOT NULL,
                price INTEGER,
                first_seen_at REAL NOT NULL,
                PRIMARY KEY (search_id, item_id)
            );
            CREATE TABLE IF NOT EXISTS kv (k TEXT PRIMARY KEY, v TEXT);
        """)
        self.db.execute("PRAGMA foreign_keys = ON")
        self.db.commit()

    def add(self, chat_id: int, url: str) -> tuple[int, bool]:
        row = self.db.execute("SELECT id FROM searches WHERE chat_id=? AND url=?", (chat_id, url)).fetchone()
        if row:
            return row["id"], False
        cur = self.db.execute("INSERT INTO searches (chat_id, url, created_at) VALUES (?,?,?)",
                              (chat_id, url, time.time()))
        self.db.commit()
        return cur.lastrowid, True

    def searches_of(self, chat_id: int):
        return self.db.execute("SELECT * FROM searches WHERE chat_id=? ORDER BY id", (chat_id,)).fetchall()

    def get(self, chat_id: int, sid: int):
        return self.db.execute("SELECT * FROM searches WHERE chat_id=? AND id=?", (chat_id, sid)).fetchone()

    def delete(self, chat_id: int, sid: int) -> bool:
        cur = self.db.execute("DELETE FROM searches WHERE chat_id=? AND id=?", (chat_id, sid))
        self.db.commit()
        return cur.rowcount > 0

    def due(self, now: float):
        return self.db.execute("SELECT * FROM searches WHERE next_check_at <= ? ORDER BY next_check_at",
                               (now,)).fetchall()

    def next_due_at(self) -> float | None:
        row = self.db.execute("SELECT MIN(next_check_at) AS t FROM searches").fetchone()
        return row["t"]

    def check_all_now(self, chat_id: int):
        self.db.execute("UPDATE searches SET next_check_at=0 WHERE chat_id=?", (chat_id,))
        self.db.commit()

    def postpone_all(self, until: float):
        self.db.execute("UPDATE searches SET next_check_at=MAX(next_check_at, ?)", (until,))
        self.db.commit()

    def seen_prices(self, sid: int) -> dict[str, int | None]:
        rows = self.db.execute("SELECT item_id, price FROM seen WHERE search_id=?", (sid,)).fetchall()
        return {r["item_id"]: r["price"] for r in rows}

    def remember(self, sid: int, items: list[dict]):
        now = time.time()
        self.db.executemany(
            "INSERT INTO seen (search_id, item_id, price, first_seen_at) VALUES (?,?,?,?) "
            "ON CONFLICT (search_id, item_id) DO UPDATE SET price=excluded.price",
            [(sid, it["id"], it["price"], now) for it in items])
        self.db.commit()

    def mark_ok(self, sid: int, title: str, count: int, next_at: float):
        self.db.execute("UPDATE searches SET primed=1, last_ok_at=?, last_error=NULL, last_count=?, "
                        "next_check_at=?, title=CASE WHEN title='' THEN ? ELSE title END WHERE id=?",
                        (time.time(), count, next_at, title, sid))
        self.db.commit()

    def mark_error(self, sid: int, err: str, next_at: float):
        self.db.execute("UPDATE searches SET last_error=?, next_check_at=? WHERE id=?", (err[:300], next_at, sid))
        self.db.commit()

    def kv_get(self, k: str, default=None):
        row = self.db.execute("SELECT v FROM kv WHERE k=?", (k,)).fetchone()
        return row["v"] if row else default

    def kv_set(self, k: str, v):
        self.db.execute("INSERT INTO kv (k, v) VALUES (?,?) ON CONFLICT (k) DO UPDATE SET v=excluded.v", (k, str(v)))
        self.db.commit()


# ── Telegram ─────────────────────────────────────────────────────────────────
class TG:
    def __init__(self, session: aiohttp.ClientSession):
        self.s = session
        self.base = f"https://api.telegram.org/bot{BOT_TOKEN}/"

    async def call(self, method: str, data=None, timeout: float = 30):
        try:
            async with self.s.post(self.base + method, data=data, proxy=TG_PROXY,
                                   timeout=aiohttp.ClientTimeout(total=timeout)) as r:
                body = await r.json(content_type=None)
                if not body.get("ok"):
                    log.warning("tg %s: %s", method, body.get("description"))
                return body
        except Exception as e:  # noqa: BLE001
            log.warning("tg %s упал: %s", method, e)
            return {"ok": False, "description": str(e)}

    async def text(self, chat_id: int, text: str):
        return await self.call("sendMessage", {"chat_id": chat_id, "text": text, "parse_mode": "HTML",
                                               "disable_web_page_preview": "true"})

    async def ad(self, chat_id: int, caption: str, photo: str | None):
        if photo:
            r = await self.call("sendPhoto", {"chat_id": chat_id, "photo": photo, "caption": caption,
                                              "parse_mode": "HTML"})
            if r.get("ok"):
                return r
        return await self.text(chat_id, caption)

    async def document(self, chat_id: int, name: str, content: bytes):
        form = aiohttp.FormData()
        form.add_field("chat_id", str(chat_id))
        form.add_field("document", content, filename=name, content_type="application/json")
        return await self.call("sendDocument", form, timeout=60)


def caption_for(it: dict, search_title: str) -> str:
    e = html.escape
    lines = [f"<b>{e(it['price_text'] or 'цена не указана')}</b>",
             f"<a href=\"{e(it['url'])}\">{e(it['title'])}</a>"]
    where = ", ".join(x for x in [it["address"], *it["metro"]] if x)
    if where:
        lines.append(f"📍 {e(where)}")
    if it["seller"]:
        lines.append(f"👤 {e(' · '.join(it['seller']))}")
    when = extract.fmt_ts(it["ts"]) or it.get("date_text", "")
    if when:
        lines.append(f"🕒 {e(when)}")
    if it["description"]:
        d = it["description"].replace("\n", " ")
        lines.append(e(d[:300] + ("…" if len(d) > 300 else "")))
    if search_title:
        lines.append(f"<i>{e(search_title[:80])}</i>")
    cap = "\n".join(lines)
    return cap[:1024]


# ── Браузер ──────────────────────────────────────────────────────────────────
def _proxy_cfg(url: str):
    if not url:
        return None
    from urllib.parse import unquote, urlparse
    u = urlparse(url)
    cfg = {"server": f"{u.scheme}://{u.hostname}" + (f":{u.port}" if u.port else "")}
    if u.username:
        cfg["username"] = unquote(u.username)
    if u.password:
        cfg["password"] = unquote(u.password)
    return cfg


async def _block_heavy(route):
    # Картинки и видео не грузим: ссылки на фото берём из данных, а трафик и
    # память рендерера на квартирной выдаче заметные. Шрифты не трогаем — их
    # отсутствие меняет отпечаток (урок wbaas).
    try:
        if route.request.resource_type in ("image", "media"):
            await route.abort()
        else:
            await route.continue_()
    except Exception:  # noqa: BLE001
        pass


class Blocked(Exception):
    pass


async def fetch_search(ctx, url: str) -> tuple[str, list[dict], list[dict]]:
    """→ (заголовок страницы, объявления, сырые объявления для /dump)."""
    page = ctx.pages[0] if ctx.pages else await ctx.new_page()
    resp = await page.goto(url, wait_until="domcontentloaded", timeout=NAV_TIMEOUT_S * 1000)
    status = resp.status if resp else None
    # Челлендж, если он есть, успевает перерисовать страницу; заодно
    # человекоподобная пауза и прокрутка.
    await page.wait_for_timeout(random.randint(1500, 3000))
    try:
        await page.mouse.move(random.randint(100, 1200), random.randint(100, 700), steps=random.randint(4, 9))
        await page.mouse.wheel(0, random.randint(300, 1200))
    except Exception:  # noqa: BLE001
        pass
    await page.wait_for_timeout(random.randint(500, 1500))

    title = await page.title()
    body = await page.evaluate("() => document.body ? document.body.innerText.slice(0, 3000) : ''")
    if extract.is_blocked(title, body, status):
        raise Blocked(f"HTTP {status}, «{title[:80]}»")

    raw = extract.raw_items_from_state(await page.evaluate(STATE_JS))
    if raw is not None:
        items = [extract.normalize(r) for r in raw]
    else:
        cards = await page.evaluate(DOM_JS)
        if not cards:
            raise RuntimeError(f"не нашёл объявлений ни в стейте, ни в DOM (HTTP {status}, «{title[:80]}»)")
        log.warning("стейта нет, взял %d карточек из DOM", len(cards))
        raw = cards
        items = [extract.normalize_dom(c) for c in cards if c.get("id")]
    try:
        await page.goto("about:blank", wait_until="commit", timeout=10000)
    except Exception:  # noqa: BLE001
        pass
    return title, items, raw


# ── Проверка ─────────────────────────────────────────────────────────────────
class Watcher:
    def __init__(self, store: Store, tg: TG):
        self.store = store
        self.tg = tg
        self.wake = asyncio.Event()
        self.block_streak = int(store.kv_get("block_streak", "0"))
        self.dumps: dict[int, list] = {}

    def _next_at(self) -> float:
        return time.time() + random.uniform(INTERVAL_MIN_S, INTERVAL_MAX_S)

    async def _notify_owners(self, text: str):
        for chat in {r["chat_id"] for r in self.store.db.execute("SELECT DISTINCT chat_id FROM searches")} or ALLOWED:
            await self.tg.text(chat, text)

    async def run(self):
        async with async_playwright() as pw:
            while True:
                due = self.store.due(time.time())
                if due:
                    await self._round(pw, due)
                (DATA_DIR / "heartbeat").write_text(str(time.time()))
                nxt = self.store.next_due_at()
                # Не дольше 5 минут: heartbeat для healthcheck пишется в этом цикле.
                wait = 300.0 if nxt is None else min(300.0, max(1.0, nxt - time.time()))
                self.wake.clear()
                try:
                    await asyncio.wait_for(self.wake.wait(), timeout=wait)
                except asyncio.TimeoutError:
                    pass

    async def _round(self, pw, due):
        profile = DATA_DIR / "profile"
        kw = {"headless": HEADLESS, "locale": "ru-RU", "timezone_id": "Europe/Moscow",
              "viewport": {"width": 1366, "height": 768},
              "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
        if proxy := _proxy_cfg(BROWSER_PROXY):
            kw["proxy"] = proxy
        ctx = None
        try:
            ctx = await pw.chromium.launch_persistent_context(str(profile), **kw)
            await ctx.route("**/*", _block_heavy)
            for i, s in enumerate(due):
                if i:
                    await asyncio.sleep(random.uniform(*BETWEEN_SEARCHES_S))
                try:
                    title, items, raw = await asyncio.wait_for(fetch_search(ctx, s["url"]), CHECK_TIMEOUT_S)
                except Blocked as b:
                    await self._on_blocked(s, str(b))
                    return
                except Exception as e:  # noqa: BLE001
                    log.warning("подписка %d: ошибка проверки: %s", s["id"], e)
                    self.store.mark_error(s["id"], str(e), self._next_at())
                    continue
                self.dumps[s["id"]] = raw[:3]
                await self._on_items(s, title, items)
        except Exception as e:  # noqa: BLE001
            log.error("раунд упал: %s", e)
            self.store.postpone_all(time.time() + 60)
        finally:
            if ctx:
                try:
                    await asyncio.wait_for(ctx.close(), 30)
                except Exception:  # noqa: BLE001
                    pass
            # Профиль стираем только после закрытия браузера, иначе он
            # допишет туда те же сгоревшие cookie.
            if self.block_streak >= WIPE_PROFILE_AFTER:
                shutil.rmtree(DATA_DIR / "profile", ignore_errors=True)
                log.warning("стёр профиль браузера после %d блоков подряд", self.block_streak)

    async def _on_blocked(self, s, why: str):
        self.block_streak += 1
        self.store.kv_set("block_streak", self.block_streak)
        backoff = min(BLOCK_BACKOFF_MAX_S, BLOCK_BACKOFF_MIN_S * 2 ** (self.block_streak - 1))
        log.warning("подписка %d: стена антибота (%s), подряд %d, пауза %.0f с",
                    s["id"], why, self.block_streak, backoff)
        self.store.mark_error(s["id"], "блок: " + why, time.time() + backoff)
        self.store.postpone_all(time.time() + backoff)
        if self.block_streak == 1:
            await self._notify_owners(f"⚠️ Авито не пускает ({html.escape(why)}). "
                                      f"Повторю через {backoff / 60:.0f} мин, дальше с нарастающей паузой.")

    async def _on_items(self, s, title: str, items: list[dict]):
        sid, chat = s["id"], s["chat_id"]
        if self.block_streak:
            await self.tg.text(chat, "✅ Авито снова пускает, проверки идут.")
            self.block_streak = 0
            self.store.kv_set("block_streak", 0)
        seen = self.store.seen_prices(sid)
        search_title = s["title"] or title

        if not s["primed"]:
            self.store.remember(sid, items)
            self.store.mark_ok(sid, title, len(items), self._next_at())
            await self.tg.text(chat, f"Подписка #{sid} работает: в выдаче сейчас {len(items)} объявл. "
                                     "Дальше пришлю только новые.")
            return

        now_ms = time.time() * 1000
        fresh, stale, cheaper = [], [], []
        for it in items:
            if it["id"] not in seen:
                too_old = it["ts"] and now_ms - it["ts"] > FRESH_WINDOW_S * 1000
                (stale if too_old else fresh).append(it)
            elif it["price"] and seen[it["id"]] and it["price"] < seen[it["id"]]:
                cheaper.append((it, seen[it["id"]]))

        # Сначала помним, потом шлём: упавший sendPhoto не должен приводить к
        # повтору той же пачки на следующей проверке.
        self.store.remember(sid, items)
        self.store.mark_ok(sid, title, len(items), self._next_at())
        if stale:
            log.info("подписка %d: %d старых объявлений всплыло в выдаче, не шлю", sid, len(stale))

        fresh.sort(key=lambda it: it["ts"] or 0)
        for it in fresh[-MAX_PER_CHECK:]:
            await self.tg.ad(chat, "🆕 " + caption_for(it, search_title), it["photo"])
        if len(fresh) > MAX_PER_CHECK:
            await self.tg.text(chat, f"…и ещё {len(fresh) - MAX_PER_CHECK} новых в подписке #{sid}: "
                                     f"{html.escape(s['url'])}")
        for it, old in cheaper[:MAX_PER_CHECK]:
            await self.tg.text(chat, f"📉 Цена снижена: {_rub(old)} → {_rub(it['price'])}\n"
                                     f"<a href=\"{html.escape(it['url'])}\">{html.escape(it['title'])}</a>")
        log.info("подписка %d: %d в выдаче, новых %d, дешевле %d", sid, len(items), len(fresh), len(cheaper))


def _rub(v: int) -> str:
    return f"{v:,} ₽".replace(",", " ")


# ── Команды ──────────────────────────────────────────────────────────────────
def _ago(ts) -> str:
    if not ts:
        return "ещё не было"
    m = int((time.time() - ts) / 60)
    return "только что" if m < 1 else f"{m} мин назад"


async def handle(msg: dict, store: Store, tg: TG, watcher: Watcher):
    chat = msg["chat"]["id"]
    text = (msg.get("text") or "").strip()
    if chat not in ALLOWED:
        await tg.text(chat, f"Бот приватный. Твой chat_id: <code>{chat}</code>")
        return
    cmd, _, arg = text.partition(" ")
    cmd = cmd.split("@")[0].lower()

    if cmd in ("/start", "/help"):
        await tg.text(chat, HELP)
    elif cmd == "/list":
        rows = store.searches_of(chat)
        if not rows:
            await tg.text(chat, "Подписок нет. Пришли ссылку на выдачу Авито.")
            return
        out = []
        for r in rows:
            name = html.escape((r["title"] or r["url"])[:80])
            out.append(f"#{r['id']} <a href=\"{html.escape(r['url'])}\">{name}</a>\n"
                       f"   проверка {_ago(r['last_ok_at'])}, в выдаче {r['last_count'] or 0}"
                       + (f"\n   ⚠️ {html.escape(r['last_error'])}" if r["last_error"] else ""))
        await tg.text(chat, "\n".join(out))
    elif cmd == "/del":
        ok = arg.strip().lstrip("#").isdigit() and store.delete(chat, int(arg.strip().lstrip("#")))
        await tg.text(chat, "Удалил." if ok else "Нет такой подписки. Номера — в /list.")
    elif cmd == "/check":
        store.check_all_now(chat)
        watcher.wake.set()
        await tg.text(chat, "Проверяю.")
    elif cmd == "/status":
        nxt = store.next_due_at()
        await tg.text(chat, f"Блоков подряд: {watcher.block_streak}\n"
                            f"Следующая проверка: {'—' if nxt is None else f'через {max(0, int(nxt - time.time()))} с'}")
    elif cmd == "/dump":
        sid = int(arg.strip().lstrip("#")) if arg.strip().lstrip("#").isdigit() else 0
        raw = watcher.dumps.get(sid)
        if not raw:
            await tg.text(chat, "Данных пока нет: дождись проверки этой подписки.")
            return
        await tg.document(chat, f"avito-{sid}.json", json.dumps(raw, ensure_ascii=False, indent=2).encode())
    elif link := next((w for w in text.split() if "avito.ru" in w), None):
        url = extract.normalize_search_url(link)
        if not url:
            await tg.text(chat, "Не похоже на ссылку Авито.")
            return
        if len(store.searches_of(chat)) >= 20:
            await tg.text(chat, "Уже 20 подписок, удали лишние через /del.")
            return
        sid, created = store.add(chat, url)
        if created:
            watcher.wake.set()
            await tg.text(chat, f"Подписка #{sid} добавлена, сейчас запомню текущую выдачу.")
        else:
            await tg.text(chat, f"Такая подписка уже есть: #{sid}.")
    else:
        await tg.text(chat, HELP)


async def poll_updates(store: Store, tg: TG, watcher: Watcher):
    offset = int(store.kv_get("tg_offset", "0"))
    while True:
        r = await tg.call("getUpdates", {"offset": offset, "timeout": 50,
                                         "allowed_updates": json.dumps(["message"])}, timeout=70)
        if not r.get("ok"):
            await asyncio.sleep(5)
            continue
        for upd in r.get("result", []):
            offset = upd["update_id"] + 1
            store.kv_set("tg_offset", offset)
            if msg := upd.get("message"):
                try:
                    await handle(msg, store, tg, watcher)
                except Exception as e:  # noqa: BLE001
                    log.exception("обработка сообщения упала: %s", e)


async def probe(url: str):
    """Разовая проверка выдачи без Telegram: python watch.py --probe <ссылка>."""
    url = extract.normalize_search_url(url) or url
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    async with async_playwright() as pw:
        kw = {"headless": HEADLESS, "locale": "ru-RU", "timezone_id": "Europe/Moscow",
              "viewport": {"width": 1366, "height": 768}, "args": ["--no-sandbox", "--disable-dev-shm-usage"]}
        if proxy := _proxy_cfg(BROWSER_PROXY):
            kw["proxy"] = proxy
        ctx = await pw.chromium.launch_persistent_context(str(DATA_DIR / "profile"), **kw)
        await ctx.route("**/*", _block_heavy)
        try:
            title, items, raw = await fetch_search(ctx, url)
        except Blocked as b:
            print(f"БЛОК: {b}")
            return
        finally:
            await ctx.close()
    (DATA_DIR / "probe.json").write_text(json.dumps(raw[:3], ensure_ascii=False, indent=2))
    print(f"«{title}»: объявлений {len(items)}, сырые первые три — {DATA_DIR / 'probe.json'}")
    for it in items[:3]:
        print(json.dumps({k: v for k, v in it.items() if k != "description"}, ensure_ascii=False))


async def main():
    if not BOT_TOKEN:
        # Не падаем: иначе restart: unless-stopped крутит контейнер при каждом
        # общем деплое, пока токен не заведён.
        log.warning("AVITO_BOT_TOKEN не задан — сервис спит")
        (DATA_DIR / "heartbeat").parent.mkdir(parents=True, exist_ok=True)
        while True:
            (DATA_DIR / "heartbeat").write_text(str(time.time()))
            await asyncio.sleep(300)
    DATA_DIR.mkdir(parents=True, exist_ok=True)
    store = Store(DATA_DIR / "avito.db")
    async with aiohttp.ClientSession() as session:
        tg = TG(session)
        watcher = Watcher(store, tg)
        log.info("старт: разрешённые чаты %s, интервал %.0f–%.0f с, прокси TG %s",
                 sorted(ALLOWED), INTERVAL_MIN_S, INTERVAL_MAX_S, TG_PROXY or "нет")
        await asyncio.gather(poll_updates(store, tg, watcher), watcher.run())


if __name__ == "__main__":
    import sys
    if len(sys.argv) == 3 and sys.argv[1] == "--probe":
        asyncio.run(probe(sys.argv[2]))
    else:
        asyncio.run(main())
