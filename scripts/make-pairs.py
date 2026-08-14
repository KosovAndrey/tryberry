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
#
# Написаний уже ЧЕТЫРЕ, и каждая площадка добавила своё:
#   «8/256 ГБ» (Ozon), «8 ГБ/256 ГБ» (Я.Маркет), «8/256» без единицы (Я.Маркет),
#   «256+8 ГБ» и «12+512 Гб» (WB — через плюс, причём порядок ОБРАТНЫЙ).
# Поэтому не плодим частные шаблоны, а разбираем любую пару чисел через «/» или
# «+» и раскладываем по величине: оперативки больше, чем ПЗУ, не бывает
# (4/64, 8/256, 12/512, 16/1024). Порядок в названии значения не имеет.
RE_MEM_PAIR = re.compile(
    r"\b(\d{1,4})\s*(?:гб|gb)?\s*[/+]\s*(\d{1,4})\s*(?:гб|gb)?\b", re.I)
# ПЯТЫЙ формат: через ПРОБЕЛ, без разделителя — «Redmi 15C 8 256 Черный».
# Найден на живой разметке: подсказка врала «память совпал» там, где 4/256
# против 8/256. Голый пробел даёт много ложных срабатываний (в названиях полно
# чисел), поэтому требуем ПРАВДОПОДОБНЫЕ значения: столько-то ОЗУ и ПЗУ реально
# бывает, а «15 8» из «Note 15 8» — нет.
RE_MEM_SPACE = re.compile(r"\b(\d{1,2})\s+(\d{2,4})\s*(?:гб|gb)?\b", re.I)
# Терабайты. «iPhone 1 ТБ» и «iPhone 1024 ГБ» — ОДИН товар, поэтому приводим к
# гигабайтам. Без этого один и тот же телефон разъезжается на два.
RE_MEM_TB = re.compile(r"\b(\d{1,2})\s*(?:тб|tb)\b", re.I)
# Пара, где ПЗУ указано в терабайтах: «12/1 ТБ», «16 ГБ/1 ТБ».
RE_MEM_PAIR_TB = re.compile(
    r"\b(\d{1,3})\s*(?:гб|gb)?\s*[/+]\s*(\d{1,2})\s*(?:тб|tb)\b", re.I)
RAM_VALUES = {2, 3, 4, 6, 8, 12, 16, 18, 24}
ROM_VALUES = {16, 32, 64, 128, 256, 512, 1024}
RE_MEM_ONE = re.compile(r"\b(\d{2,4})\s*(?:гб|gb)\b", re.I)
RE_TOKEN = re.compile(r"[a-zA-Zа-яА-ЯёЁ0-9]+")

# Признаки, по которым товар с тем же названием — ДРУГОЙ товар. Найдены в первой
# же выборке: «Восстановленный iPhone 14 Pro Max» против нового, «Global» против
# «Ростест (EAC)». Для покупателя это разные вещи (гарантия, состояние), и
# матчер обязан их различать.
#
# Границы слов обязательны: короткое «cn» подстрокой находится внутри «Tecno»,
# и первая версия честно рапортовала китайскую версию у каждого Tecno Camon.
MARKERS = {
    "состояние": [r"восстановлен", r"уценённ", r"уцененн", r"\bб/?у\b", r"refurb"],
    "версия": [r"ростест", r"\beac\b", r"\bglobal\b", r"глобальн",
               r"китайск", r"\bcn\b"],
    "комплект": [r"комплект", r"\bнабор", r"\b[23]\s*шт\b"],
}
MARKERS = {k: [re.compile(p, re.I) for p in v] for k, v in MARKERS.items()}


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
    """Объём памяти в ГИГАБАЙТАХ: (ram, rom) либо (None, rom) либо None."""
    m = RE_MEM_PAIR_TB.search(name)
    if m:
        return (int(m.group(1)), int(m.group(2)) * 1024)
    m = RE_MEM_PAIR.search(name)
    if m:
        a, b = int(m.group(1)), int(m.group(2))
        return (min(a, b), max(a, b))
    for m in RE_MEM_SPACE.finditer(name):
        a, b = int(m.group(1)), int(m.group(2))
        if a in RAM_VALUES and b in ROM_VALUES:
            return (a, b)
    m = RE_MEM_TB.search(name)
    if m:
        return (None, int(m.group(1)) * 1024)
    m = RE_MEM_ONE.search(name)
    if m:
        return (None, int(m.group(1)))
    return None


# Квалификаторы имени модели. Тот же тип ошибки, что объём памяти, только по
# названию: «Pro» против «Pro Max», «Note 13» против «13», «Lite» против базовой
# — это РАЗНЫЕ товары с почти одинаковыми названиями, и решаться должно жёстко,
# а не похожестью. Идея пользователя по итогам живой разметки.
#
# Сюда идут только те слова, которые реально образуют отдельную модель. «Смартфон»
# и цвета — не квалификаторы, они шум.
VARIANT_WORDS = {
    "pro", "про", "max", "макс", "plus", "плюс", "ultra", "ультра",
    "lite", "лайт", "mini", "мини", "note", "нот", "neo", "se", "fe",
    "prime", "power", "turbo", "active", "young",
}
# «5G» СПЕЦИАЛЬНО не входит: площадки его часто просто опускают, поэтому
# отсутствие не означает отличия — ведёт себя как «Ростест», а не как «Pro».
# Выяснилось на живой разметке: пара «Note 15 Pro 5G» против «Note 15 Pro»
# размечена как один товар, и это единственное противоречие метки подсказке.


# Плюс ПОСЛЕ БУКВЫ — часть имени модели: «Note 15 Pro+» и «Note 15 Pro» разные
# телефоны. Плюс между цифрами («8+256») — объём памяти, его сюда пускать нельзя.
RE_PLUS_SUFFIX = re.compile(r"([a-zA-Zа-яА-ЯёЁ]{2,})\s*\+", re.I)


def variant_of(name: str):
    """Множество квалификаторов модели в названии."""
    out = {t for t in (x.lower() for x in RE_TOKEN.findall(name))
           if t in VARIANT_WORDS}
    for m in RE_PLUS_SUFFIX.finditer(name):
        w = m.group(1).lower()
        if w in VARIANT_WORDS:
            out.add(w + "+")
    return out


def variant_verdict(a: str, b: str) -> str:
    """Есть ли у одного квалификатор, которого нет у другого."""
    va, vb = variant_of(a), variant_of(b)
    if va == vb:
        return "совпал" if va else "нет"
    diff = sorted(va ^ vb)
    return "РАЗНЫЙ:" + "/".join(diff)


def mem_verdict(a, b) -> str:
    """Сравниваем по ПЗУ: оперативку одна площадка часто не пишет, и требовать
    её совпадения значит объявить разными объёмы, которые совпадают."""
    if not a or not b:
        return "нет"
    if a[1] != b[1]:
        return "РАЗНЫЙ"
    if a[0] is not None and b[0] is not None and a[0] != b[0]:
        return "РАЗНЫЙ"
    return "совпал"


def markers_of(name: str):
    found = []
    for kind, patterns in MARKERS.items():
        hits = sorted({m.group(0).lower() for p in patterns
                       for m in [p.search(name)] if m})
        if hits:
            found.append(f"{kind}:{'/'.join(hits)}")
    return found


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
    ap.add_argument("--hard-share", type=float, default=0.35,
                    help="минимальная доля трудных негативов (разный объём памяти)")
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

    # Отбор с КВОТОЙ на трудные негативы. Без неё верхушку по схожести занимают
    # лёгкие совпадения, и главный тип ошибки (тот же телефон, другой объём —
    # §9, тип C) оказывается представлен одной шестой набора. Мерить точность
    # там, где опасных случаев мало, значит мерить не то: разметка дорогая и
    # делается один раз.
    ranked = sorted(scored, key=lambda x: -x[0])
    hard, rest, seen = [], [], set()
    for s, ua, ub in ranked:
        key = tuple(sorted((ua, ub)))
        if key in seen:
            continue
        seen.add(key)
        a, b = by_url[ua], by_url[ub]
        (hard if mem_verdict(a["_mem"], b["_mem"]) == "РАЗНЫЙ" else rest).append((s, a, b))

    want_hard = min(len(hard), int(args.limit * args.hard_share))
    pairs = hard[:want_hard] + rest[: args.limit - want_hard]
    pairs.sort(key=lambda x: -x[0])
    print(f"трудных негативов доступно: {len(hard)}, взято: {want_hard}",
          file=sys.stderr)

    tsv_path, txt_path = args.out + ".tsv", args.out + ".txt"
    with open(tsv_path, "w", encoding="utf-8") as tsv, \
            open(txt_path, "w", encoding="utf-8") as txt:
        tsv.write("label\tid\tsim\tmem\tflags\tbrand\tmp_a\tname_a\tprice_a\tmp_b"
                  "\tname_b\tprice_b\turl_a\turl_b\tvariant\n")
        for i, (s, a, b) in enumerate(pairs, 1):
            mem = mem_verdict(a["_mem"], b["_mem"])
            ma, mb = markers_of(a["name"]), markers_of(b["name"])
            flags = "; ".join(f"A[{x}]" for x in ma) + \
                    (" " if ma and mb else "") + \
                    "; ".join(f"B[{x}]" for x in mb)
            var = variant_verdict(a["name"], b["name"])
            tsv.write(f"\t{i}\t{s:.3f}\t{mem}\t{flags}\t{a['_brand']}\t"
                      f"{a['marketplace']}\t{a['name']}\t{a['price']}\t"
                      f"{b['marketplace']}\t{b['name']}\t{b['price']}\t"
                      f"{a['url']}\t{b['url']}\t{var}\n")
            txt.write(f"[{i:3d}] sim={s:.3f} бренд={a['_brand']} память={mem}"
                      f"{'  ⚑ ' + flags if flags else ''}\n"
                      f"   A ({a['marketplace']}, {a['price']} ₽) {a['name']}\n"
                      f"   B ({b['marketplace']}, {b['price']} ₽) {b['name']}\n"
                      f"   {a['url']}\n   {b['url']}\n\n")

    same = sum(1 for s, a, b in pairs if mem_verdict(a["_mem"], b["_mem"]) == "совпал")
    diff = sum(1 for s, a, b in pairs if mem_verdict(a["_mem"], b["_mem"]) == "РАЗНЫЙ")
    print(f"пар на разметку: {len(pairs)} → {tsv_path}, {txt_path}", file=sys.stderr)
    print(f"  из них с разным объёмом памяти (трудные негативы): {diff}", file=sys.stderr)
    print(f"  с совпавшим объёмом: {same}", file=sys.stderr)
    print("\nРазметка: в колонку label ставить 1 (тот же товар) или 0 (разные).",
          file=sys.stderr)


if __name__ == "__main__":
    main()
