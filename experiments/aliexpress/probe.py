#!/usr/bin/env python3
"""
Camoufox-проб для aliexpress.ru.

Цель эксперимента: проверить, проходит ли антидетект-браузер (Camoufox/Firefox)
антибот X5SEC («punish»/captcha с x5secdata) на странице товара — с ЧИСТОГО
датацентр-IP VPS, БЕЗ прокси. Голый HTTP (даже с Chrome-JA3 через tls-client)
этот челлендж не проходит — отдаёт punish-страницу.

Что делает:
  1. Открывает https://aliexpress.ru/item/<ID>.html в Camoufox (headless).
  2. Перехватывает сетевые ответы, похожие на API цены (mtop / aer-api / pdp /
     price / /fn/), и сохраняет их тела — это будущая цель парсера.
  3. Ждёт, пока отработает JS (включая авто-прохождение челленджа), снимает
     отрендеренный HTML + скриншот.
  4. Печатает вердикт: punish/captcha или реальная карточка (есть ли цена «₽»,
     заголовок, маркеры), и куда сохранил артефакты.

Запуск (см. run_probe.sh — он всё ставит и зовёт это):
    ALI_ID=1005005863682926 OUT_DIR=/tmp/aliprobe python probe.py

Переменные:
    ALI_ID        — артикул товара (по умолчанию демо-товар)
    OUT_DIR       — куда складывать артефакты (по умолчанию /tmp/aliprobe)
    HEADLESS_MODE — "virtual" (Xvfb, лучшая скрытность; нужен пакет xvfb) |
                    "true" (нативный headless Firefox) | "false" (видимое окно)
    ALI_PROXY     — опционально http://user:pass@host:port (по умолчанию БЕЗ прокси)
    WAIT_MS       — сколько ждать после загрузки под JS/челлендж (по умолч. 9000)
"""
import asyncio
import os
import shutil
import sys

from camoufox.async_api import AsyncCamoufox

ALI_ID = os.environ.get("ALI_ID", "1005005863682926")
URL = f"https://aliexpress.ru/item/{ALI_ID}.html"
OUT_DIR = os.environ.get("OUT_DIR", "/tmp/aliprobe")
HEADLESS_MODE = os.environ.get("HEADLESS_MODE", "virtual")
ALI_PROXY = os.environ.get("ALI_PROXY", "")
WAIT_MS = int(os.environ.get("WAIT_MS", "9000"))

# Подстроки URL, по которым ловим потенциальный API цены.
API_HINTS = ("mtop", "aer-api", "/pdp", "pdp.pc", "price", "/fn/", "detail")


def _headless_value():
    m = HEADLESS_MODE.lower()
    if m == "virtual":
        # virtual-режим требует Xvfb; если его нет — авто-фолбэк на нативный
        # headless, чтобы проб не падал (скрытность чуть ниже, но для проверки
        # «проходит ли вообще» годится).
        if shutil.which("Xvfb"):
            return "virtual"
        print("  [!] Xvfb не найден → фолбэк на нативный headless "
              "(для лучшей скрытности: sudo apt-get install -y xvfb)")
        return True
    if m == "false":
        return False
    return True


async def main():
    os.makedirs(OUT_DIR, exist_ok=True)
    captured = []  # список (url, status, len, saved_path|None)

    async def on_response(response):
        try:
            url = response.url
            if not any(h in url for h in API_HINTS):
                return
            try:
                body = await response.body()
            except Exception:
                body = b""
            text = body.decode("utf-8", "replace")
            saved = None
            # Сохраняем только тела, где реально пахнет ценой/JSON карточки —
            # чтобы не утонуть в статике.
            low = text.lower()
            if ("price" in low or "pdp" in url) and len(text) > 50:
                saved = os.path.join(OUT_DIR, f"resp_{len(captured)+1}.json")
                with open(saved, "w", encoding="utf-8") as f:
                    f.write(text)
            captured.append((url, response.status, len(text), saved))
        except Exception as e:  # перехват не должен ронять проб
            print("  [resp handler error]", e, file=sys.stderr)

    kwargs = dict(
        headless=_headless_value(),
        geoip=True,
        humanize=True,
        locale="ru-RU",
        os="windows",
    )
    if ALI_PROXY:
        kwargs["proxy"] = {"server": ALI_PROXY}

    print(f"URL:      {URL}")
    print(f"headless: {HEADLESS_MODE}   proxy: {bool(ALI_PROXY)}")
    print("launching camoufox...")

    async with AsyncCamoufox(**kwargs) as browser:
        page = await browser.new_page()
        page.on("response", lambda r: asyncio.ensure_future(on_response(r)))

        try:
            await page.goto(URL, wait_until="domcontentloaded", timeout=60000)
        except Exception as e:
            print("goto error:", e)

        # Даём время на JS: авто-прохождение челленджа + дозагрузку данных.
        await page.wait_for_timeout(WAIT_MS)

        # Если нас увело на punish и там нет интерактива — иногда помогает один
        # реход на исходный URL уже с выданными челленджем cookie.
        content = await page.content()
        if "x5secdata" in content or "/punish" in page.url:
            print("  punish detected on first load → retry once...")
            try:
                await page.goto(URL, wait_until="domcontentloaded", timeout=60000)
                await page.wait_for_timeout(WAIT_MS)
                content = await page.content()
            except Exception as e:
                print("  retry error:", e)

        title = await page.title()
        final = page.url

        html_path = os.path.join(OUT_DIR, "page.html")
        with open(html_path, "w", encoding="utf-8") as f:
            f.write(content)
        png_path = os.path.join(OUT_DIR, "page.png")
        try:
            await page.screenshot(path=png_path, full_page=False)
        except Exception as e:
            print("screenshot error:", e)
            png_path = "(none)"

        # Маркеры в отрендеренном DOM.
        markers = {}
        for k in (
            "₽", '"price"', "sellPrice", "formatedPrice", "skuPriceList",
            "window.runParams", "_init_data_", "__INITIAL",
            "x5secdata", "_____tmd_____", "punish", "captcha", "slider",
        ):
            markers[k] = content.count(k)

        # Глобальные объекты с данными карточки — пригодится парсеру.
        try:
            globals_found = await page.evaluate(
                "() => Object.keys(window).filter(k => /run|init|state|data|pdp|sku|price/i.test(k)).slice(0,40)"
            )
        except Exception:
            globals_found = []

        punished = markers["x5secdata"] > 0 or markers["punish"] > 0 or "/punish" in final
        has_price = markers["₽"] > 0 or markers['"price"'] > 0 or markers["sellPrice"] > 0

    # ── Отчёт ───────────────────────────────────────────────────────────────
    print("\n================= ВЕРДИКТ =================")
    print(f"final url: {final}")
    print(f"title:     {title!r}")
    print(f"html size: {len(content)} bytes")
    print(f"punish/captcha: {'ДА ❌' if punished else 'нет ✅'}")
    print(f"цена на странице: {'НАЙДЕНА ✅' if has_price else 'нет ❌'}")
    print("\n--- маркеры (в отрендеренном DOM) ---")
    for k, v in markers.items():
        print(f"{v:4d}  {k}")
    print("\n--- глобальные объекты window с данными ---")
    print(globals_found)
    print("\n--- перехваченные API-ответы ---")
    if not captured:
        print("(ничего не поймано по хинтам)", API_HINTS)
    for url, status, ln, saved in captured:
        tag = f"  → {saved}" if saved else ""
        print(f"[{status}] len={ln:7d}  {url[:110]}{tag}")
    print("\n--- артефакты ---")
    print(f"HTML:       {html_path}")
    print(f"screenshot: {png_path}")
    print(f"API-тела:   {OUT_DIR}/resp_*.json")
    print("==========================================")


if __name__ == "__main__":
    asyncio.run(main())
