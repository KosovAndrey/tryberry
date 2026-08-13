#!/usr/bin/env python3
"""Кандидаты в пары «тот же товар» для ручной разметки золотого набора.

Вход — TSV от cmd/title-probe (marketplace, article, brand, price, name, url).
Выход — pairs.tsv с пустой колонкой label под разметку и pairs.txt для чтения
глазами. См. docs/PRODUCT-MATCH-JVM.md §10.

    python3 scripts/make-pairs.py items.tsv -o pairs

Смысл в том, чтобы НЕ размечать случайные пары: 99% из них очевидно разные и
человеческое время на них тратится впустую. Размечать надо ГРАНИЦУ РЕШЕНИЯ —
пары, похожие настолько, что их легко перепутать. Поэтому здесь блокинг по
бренду и отбор верхушки по схожести: туда попадают и настоящие совпадения, и
трудные негативы (та же модель, другой объём памяти) — ровно то, на чём
ломается наивный матчинг.

Схожесть здесь СПЕЦИАЛЬНО примитивная (Жаккар по токенам). Это не кандидат в
боевой скоринг, а сито для разметки: если сито будет умным, набор окажется
подогнан под будущую модель и перестанет быть честной проверкой.
"""
import argparse
import re
import sys
from collections import defaultdict
from itertools import combinations

# Бренд — жёсткий ключ блокинга. Площадки пишут его и латиницей, и кириллицей,
# поэтому нормализуем словарём: без этого «Сяоми» и «Xiaomi» не встретятся.
BRAND_ALIASES = {
    "xiaomi": "xiaomi", "сяоми": "xiaomi", "ксиаоми": "xiaomi", "ксяоми": "xiaomi",
    "redmi": "xiaomi", "редми": "xiaomi", "poco": "xiaomi", "поко": "xiaomi",
    "samsung": "samsung", "самсунг": "samsung", "galaxy": "samsung",
    "apple": "apple", "эппл": "apple", "айфон": "apple", "iphone": "apple",
    "honor": "honor", "хонор": "honor",
    "huawei": "huawei", "хуавей": "huawei", "хуавэй": "huawei",
    "realme": "realme", "реалми": "realme",
    "tecno": "tecno", "текно": "tecno",
    "infinix": "infinix", "инфиникс": "infinix",
    "vivo": "vivo", "виво": "vivo",
    "oppo": "oppo", "оппо": "oppo",
    "zte": "zte", "нубиа": "zte", "nubia": "zte",
    "motorola": "motorola", "моторола": "motorola",
    "nothing": "nothing", "google": "google", "pixel": "google",
}

# Слова, не несущие идентичности: они есть почти в каждом названии и только
# завышают схожесть.
STOP = {
    "смартфон", "смартфоны", "телефон", "мобильный", "сотовый",
    "nano", "sim", "nanosim", "dual", "ростест", "eac", "версия", "global",
    "глобальная", "новый", "гарантия", "гб", "gb", "тб", "tb", "ram", "rom",
    "и", "с", "для", "в", "на",
}

# Квалификатор варианта: «8/128 ГБ», «128 ГБ». Несовпадение — это ОТКАЗ, а не
# минус к скору: у вариантов одного товара названия совпадают почти дословно,
# и именно на них ломается любой порог по похожести (§9 дока).
RE_MEM_PAIR = re.compile(r"\b(\d{1,2})\s*/\s*(\d{2,4})\s*(?:гб|gb)\b", re.I)
RE_MEM_ONE = re.compile(r"\b(\d{2,4})\s*(?:гб|gb)\b", re.I)
RE_TOKEN = re.compile(r"[a-zA-Zа-яА-ЯёЁ0-9]+")


def norm_tokens(name: str):
    toks = [t.lower() for t in RE_TOKEN.findall(name)]
    return [t for t in toks if t not in STOP and len(t) > 1]


def brand_of(row) -> str:
    """Бренд из колонки, иначе — первый узнаваемый токен названия."""
    b = (row["brand"] or "").strip().lower()
    if b in BRAND_ALIASES:
        return BRAND_ALIASES[b]
    for t in norm_tokens(row["name"]):
        if t in BRAND_ALIASES:
            return BRAND_ALIASES[t]
    return b or "?"


def storage_of(name: str):
    """Объём памяти: (ram, rom) либо (None, rom) либо None."""
    m = RE_MEM_PAIR.search(name)
    if m:
        return (int(m.group(1)), int(m.group(2)))
    m = RE_MEM_ONE.search(name)
    if m:
        return (None, int(m.group(1)))
    return None


def jaccard(a: set, b: set) -> float:
    if not a or not b:
        return 0.0
    return len(a & b) / len(a | b)


def main():
    ap = argparse.ArgumentParser()
    ap.add_argument("items", help="TSV от cmd/title-probe")
    ap.add_argument("-o", "--out", default="pairs", help="префикс выходных файлов")
    ap.add_argument("--top", type=int, default=3, help="кандидатов на товар")
    ap.add_argument("--min-sim", type=float, default=0.25, help="порог отсечки")
    ap.add_argument("--limit", type=int, default=300, help="сколько пар отдать на разметку")
    args = ap.parse_args()

    rows = []
    with open(args.items, encoding="utf-8") as f:
        header = f.readline().rstrip("\n").split("\t")
        for line in f:
            parts = line.rstrip("\n").split("\t")
            if len(parts) != len(header):
                continue
            rows.append(dict(zip(header, parts)))

    for r in rows:
        r["_brand"] = brand_of(r)
        r["_toks"] = set(norm_tokens(r["name"]))
        r["_mem"] = storage_of(r["name"])

    by_brand = defaultdict(list)
    for r in rows:
        if r["_brand"] != "?":
            by_brand[r["_brand"]].append(r)

    print(f"позиций: {len(rows)}, с распознанным брендом: "
          f"{sum(len(v) for v in by_brand.values())}, брендов: {len(by_brand)}",
          file=sys.stderr)

    scored = []
    for brand, items in by_brand.items():
        best = defaultdict(list)
        for a, b in combinations(items, 2):
            if a["marketplace"] == b["marketplace"]:
                continue  # пары ищем МЕЖДУ площадками
            s = jaccard(a["_toks"], b["_toks"])
            if s < args.min_sim:
                continue
            best[id(a)].append((s, a, b))
            best[id(b)].append((s, a, b))
        keep = set()
        for lst in best.values():
            for s, a, b in sorted(lst, key=lambda x: -x[0])[: args.top]:
                keep.add((s, id(a), id(b), a["url"], b["url"]))
        for s, _ia, _ib, ua, ub in keep:
            scored.append((s, ua, ub))

    by_url = {r["url"]: r for r in rows}
    seen, pairs = set(), []
    for s, ua, ub in sorted(scored, key=lambda x: -x[0]):
        key = tuple(sorted((ua, ub)))
        if key in seen:
            continue
        seen.add(key)
        pairs.append((s, by_url[ua], by_url[ub]))
        if len(pairs) >= args.limit:
            break

    tsv_path, txt_path = args.out + ".tsv", args.out + ".txt"
    with open(tsv_path, "w", encoding="utf-8") as tsv, \
            open(txt_path, "w", encoding="utf-8") as txt:
        tsv.write("label\tid\tsim\tmem\tbrand\tmp_a\tname_a\tprice_a\tmp_b\tname_b"
                  "\tprice_b\turl_a\turl_b\n")
        for i, (s, a, b) in enumerate(pairs, 1):
            if a["_mem"] and b["_mem"]:
                mem = "совпал" if a["_mem"] == b["_mem"] else "РАЗНЫЙ"
            else:
                mem = "нет"
            tsv.write(f"\t{i}\t{s:.3f}\t{mem}\t{a['_brand']}\t{a['marketplace']}\t"
                      f"{a['name']}\t{a['price']}\t{b['marketplace']}\t{b['name']}\t"
                      f"{b['price']}\t{a['url']}\t{b['url']}\n")
            txt.write(f"[{i:3d}] sim={s:.3f} бренд={a['_brand']} память={mem}\n"
                      f"   A ({a['marketplace']}, {a['price']} ₽) {a['name']}\n"
                      f"   B ({b['marketplace']}, {b['price']} ₽) {b['name']}\n"
                      f"   {a['url']}\n   {b['url']}\n\n")

    same = sum(1 for s, a, b in pairs
               if a["_mem"] and b["_mem"] and a["_mem"] == b["_mem"])
    diff = sum(1 for s, a, b in pairs
               if a["_mem"] and b["_mem"] and a["_mem"] != b["_mem"])
    print(f"пар на разметку: {len(pairs)} → {tsv_path}, {txt_path}", file=sys.stderr)
    print(f"  из них с разным объёмом памяти (трудные негативы): {diff}", file=sys.stderr)
    print(f"  с совпавшим объёмом: {same}", file=sys.stderr)
    print("\nРазметка: в колонку label ставить 1 (тот же товар) или 0 (разные).",
          file=sys.stderr)


if __name__ == "__main__":
    main()
