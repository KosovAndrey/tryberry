"""extract.py — разбор выдачи Авито в плоские объявления.

Чистые функции без браузера, чтобы их можно было проверять на сохранённых
страницах. Основной источник — SSR-стейт страницы: <script type="mime/invalid"
data-mfe-state="true">, внутри JSON с каталогом (тот же, что читает
Duff89/parser_avito, v3.2.21 от 12-08-2026). Запасной путь — DOM-карточки
[data-marker="item"], их собирает сам браузер (см. DOM_JS в watch.py).
"""

import html
import json
import re
from datetime import datetime, timedelta, timezone
from urllib.parse import parse_qsl, urlencode, urlparse, urlunparse

MSK = timezone(timedelta(hours=3))

# Признаки стены антибота в заголовке или тексте страницы. 17-09 домашний IP
# после двух голых запросов получил 429 с заголовком «Доступ ограничен:
# проблема с IP», мобильное API — тот же файрвол.
BLOCK_MARKERS = ("доступ ограничен", "проблема с ip", "не робот", "firewall")


def is_blocked(title: str, body_text: str, status: int | None) -> bool:
    if status in (403, 429):
        return True
    hay = f"{title}\n{body_text[:3000]}".lower()
    return any(m in hay for m in BLOCK_MARKERS)


def normalize_search_url(raw: str) -> str | None:
    """Ссылка на выдачу → канон: www.avito.ru, сортировка по дате, первая страница.

    None, если это не ссылка на Авито."""
    raw = raw.strip()
    if not raw.startswith(("http://", "https://")):
        raw = "https://" + raw
    u = urlparse(raw)
    host = (u.hostname or "").lower()
    if host not in ("avito.ru", "www.avito.ru", "m.avito.ru"):
        return None
    q = [(k, v) for k, v in parse_qsl(u.query, keep_blank_values=True) if k not in ("s", "p", "context")]
    # s=104 — «по дате»: без неё новые объявления тонут под продвинутыми.
    q.append(("s", "104"))
    return urlunparse(("https", "www.avito.ru", u.path or "/", "", urlencode(q), ""))


def _states(scripts: list[str]) -> list[dict]:
    out = []
    for text in scripts:
        if "sandbox" in text[:200]:
            continue
        try:
            out.append(json.loads(html.unescape(text)))
        except (ValueError, TypeError):
            continue
    return out


def _find_item_lists(node, found: list):
    if isinstance(node, dict):
        items = node.get("items")
        if isinstance(items, list) and any(
            isinstance(i, dict) and i.get("id") and i.get("urlPath") for i in items
        ):
            found.append(items)
        for v in node.values():
            _find_item_lists(v, found)
    elif isinstance(node, list):
        for v in node:
            _find_item_lists(v, found)


def raw_items_from_state(scripts: list[str]) -> list[dict] | None:
    """Сырые объявления из SSR-стейта. None — стейта с каталогом на странице нет
    (тогда пробуем DOM), [] — каталог есть, но пустой."""
    states = _states(scripts)
    lists: list = []
    for st in states:
        _find_item_lists(st, lists)
    if lists:
        best = max(lists, key=len)
        return [i for i in best if isinstance(i, dict) and i.get("id") and i.get("urlPath")]
    # Каталог без объявлений: ключ есть, список пуст.
    for st in states:
        if '"catalog"' in json.dumps(st, ensure_ascii=False)[:200000]:
            return []
    return None


def _largest_image(images) -> str | None:
    if not isinstance(images, list) or not images:
        return None
    first = images[0]
    if not isinstance(first, dict) or not first:
        return None

    def area(key: str) -> int:
        m = re.match(r"(\d+)x(\d+)", key)
        return int(m.group(1)) * int(m.group(2)) if m else 0

    return first[max(first, key=area)]


# Шаги iva, в которых нет ничего про продавца: дата, цена, адрес и кнопки.
_IVA_SKIP = ("DateInfoStep", "PriceStep", "GeoStep", "BadgeBarStep", "SnippetBadgesStep")
_TEXT_KEYS = ("text", "title", "value", "name", "label", "score", "rating", "reviewCount", "summary")


def _iva_strings(iva) -> list[str]:
    """Короткие строки из блоков iva: имя продавца, «Собственник», рейтинг.

    Структура iva у Авито плавает, поэтому берём строки по знакомым ключам, а не
    по точному пути. Калибровать по /dump."""
    out: list[str] = []

    def walk(node):
        if isinstance(node, dict):
            for k, v in node.items():
                if k in _TEXT_KEYS and isinstance(v, (str, int, float)) and not isinstance(v, bool):
                    s = str(v).strip()
                    if 0 < len(s) <= 60 and s not in out and not s.startswith(("http", "/")):
                        out.append(s)
                else:
                    walk(v)
        elif isinstance(node, list):
            for v in node:
                walk(v)

    if isinstance(iva, dict):
        for step, val in iva.items():
            if step not in _IVA_SKIP:
                walk(val)
    return out[:6]


def normalize(raw: dict) -> dict:
    price = raw.get("priceDetailed") or {}
    geo = raw.get("geo") or {}
    refs = []
    for r in geo.get("geoReferences") or []:
        if isinstance(r, dict):
            content = r.get("content")
            after = r.get("after")
            if content:
                refs.append(f"{content} {after}".strip() if after else content)
    address = geo.get("formattedAddress") or (raw.get("addressDetailed") or {}).get("locationName") or ""
    ts = raw.get("sortTimeStamp")
    return {
        "id": str(raw.get("id")),
        "title": raw.get("title") or "",
        "url": "https://www.avito.ru" + raw["urlPath"] if str(raw.get("urlPath", "")).startswith("/") else raw.get("urlPath"),
        "price": price.get("value") if isinstance(price.get("value"), int) else None,
        "price_text": price.get("fullString") or price.get("string") or raw.get("normalizedPrice") or "",
        "ts": ts if isinstance(ts, int) else None,
        "address": address,
        "metro": refs[:2],
        "photo": _largest_image(raw.get("images")),
        "seller": _iva_strings(raw.get("iva")),
        "description": (raw.get("description") or "").strip(),
    }


def normalize_dom(card: dict) -> dict:
    price = card.get("price")
    try:
        price = int(price) if price not in (None, "") else None
    except ValueError:
        price = None
    url = card.get("url") or ""
    if url.startswith("/"):
        url = "https://www.avito.ru" + url
    return {
        "id": str(card.get("id")),
        "title": card.get("title") or "",
        "url": url,
        "price": price,
        "price_text": f"{price:,} ₽".replace(",", " ") if price else "",
        "ts": None,
        "address": card.get("address") or "",
        "metro": [],
        "photo": card.get("photo"),
        "seller": [s for s in (card.get("seller") or []) if s][:4],
        "description": (card.get("description") or "").strip(),
        "date_text": card.get("date") or "",
    }


def fmt_ts(ts_ms: int | None) -> str:
    if not ts_ms:
        return ""
    return datetime.fromtimestamp(ts_ms / 1000, MSK).strftime("%d.%m %H:%M")
