#!/usr/bin/env python3
"""Кто прав там, где триграммы и индекс расходятся — решающее сравнение.

    python3 scripts/compare-methods.py \\
        testdata/product-match/items-smartphones.tsv \\
        testdata/product-match/disagreement-smartphones.tsv \\
        /tmp/lucene-scored.tsv

Отвечает на вопрос, ради которого собирался батч и записывалось условие отказа
(docs/PRODUCT-MATCH-JVM.md §10): даёт ли индекс достаточный отрыв от честного
триграммного baseline, чтобы оправдать отдельный сервис.

ЧТО ЗДЕСЬ ЧЕСТНО СЧИТАЕТСЯ:
  - на парах РАСХОЖДЕНИЯ: кто из методов оказался прав чаще;
  - на контрольной группе СОГЛАСИЯ: можно ли вообще верить их согласию.

ЧЕГО СЧИТАТЬ НЕЛЬЗЯ: абсолютных precision и recall по каталогу. Выборка смещена
по построению — в неё специально отбирались спорные пары. Для абсолютных чисел
нужна случайная выборка товаров, её у нас нет.
"""
import sys
from collections import defaultdict
from importlib.machinery import SourceFileLoader

_base = __file__.rsplit("/", 1)[0]
trgm = SourceFileLoader("trgm", _base + "/trgm-baseline.py").load_module()
mp = SourceFileLoader("mp", _base + "/make-pairs.py").load_module()

TOP_N = 3


def load(path):
    with open(path, encoding="utf-8") as f:
        head = f.readline().rstrip("\n").split("\t")
        rows = [r for r in (ln.rstrip("\n").split("\t") for ln in f) if len(r) == len(head)]
    return head, rows, {n: i for i, n in enumerate(head)}


def main():
    if len(sys.argv) < 4:
        print(__doc__)
        sys.exit(2)
    items_path, pairs_path, scored_path = sys.argv[1:4]

    ihead, irows, ii = load(items_path)
    items = [dict(zip(ihead, r)) for r in irows]
    by_url = {it["url"]: it for it in items}
    for it in items:
        it["_brand"] = mp.brand_of(it)
    by_brand = defaultdict(list)
    for it in items:
        by_brand[it["_brand"]].append(it)

    _sh, srows, si = load(scored_path)
    lucene_rank = {r[si["id"]]: int(r[si["lucene_rank"]]) for r in srows}

    _ph, prows, pi = load(pairs_path)

    def trgm_rank(a, b):
        cands = sorted(((trgm.similarity(a["name"], c["name"]), c["url"])
                        for c in by_brand[a["_brand"]]
                        if c["marketplace"] != a["marketplace"]), key=lambda x: -x[0])
        for pos, (_s, url) in enumerate(cands[:TOP_N], 1):
            if url == b["url"]:
                return pos
        return 0

    dis = {"lucene": 0, "trgm": 0}          # кто оказался прав на расхождениях
    dis_detail = []
    agree_ok = agree_bad = 0
    agree_found_ok = agree_found_bad = 0
    labeled = 0

    for r in prows:
        lab = r[pi["label"]].strip()
        if lab not in ("0", "1"):
            continue
        a, b = by_url.get(r[pi["url_a"]]), by_url.get(r[pi["url_b"]])
        if not a or not b:
            continue
        labeled += 1
        t = min([x for x in (trgm_rank(a, b), trgm_rank(b, a)) if x] or [0])
        l = lucene_rank.get(r[pi["id"]], 0)
        t_top, l_top = 0 < t <= TOP_N, 0 < l <= TOP_N
        same = lab == "1"

        if t_top == l_top:
            # Согласие: проверяем, заслуживает ли оно доверия.
            if t_top:
                agree_found_ok += same
                agree_found_bad += (not same)
            else:
                agree_ok += (not same)
                agree_bad += same
            continue

        # Расхождение: прав тот, чьё решение совпало с меткой.
        winner = "lucene" if (l_top == same) else "trgm"
        dis[winner] += 1
        dis_detail.append((winner, lab, r, t, l))

    print(f"размеченных пар в батче: {labeled}")
    print()
    print("═══ РАСХОЖДЕНИЯ: кто прав ═══")
    total = dis["lucene"] + dis["trgm"]
    if total:
        print(f"  индекс прав:    {dis['lucene']:3d}  ({100*dis['lucene']/total:.0f} %)")
        print(f"  триграммы правы: {dis['trgm']:3d}  ({100*dis['trgm']/total:.0f} %)")
        print(f"  всего спорных:  {total}")
    else:
        print("  расхождений среди размеченных нет")

    print()
    print("═══ СОГЛАСИЕ: можно ли ему верить ═══")
    both_found = agree_found_ok + agree_found_bad
    both_missed = agree_ok + agree_bad
    if both_found:
        print(f"  оба нашли:    {both_found:3d}, из них верно {agree_found_ok} "
              f"({100*agree_found_ok/both_found:.0f} %)")
    if both_missed:
        print(f"  оба не нашли: {both_missed:3d}, из них верно {agree_ok} "
              f"({100*agree_ok/both_missed:.0f} %)")

    print()
    print("═══ Разбор: где индекс оказался прав, а триграммы нет ═══")
    shown = 0
    for winner, lab, r, t, l in dis_detail:
        if winner != "lucene" or shown >= 6:
            continue
        shown += 1
        print(f"  метка={lab} ранг: индекс={l} триграммы={t or '—'}")
        print(f"    A {r[pi['name_a']][:96]}")
        print(f"    B {r[pi['name_b']][:96]}")

    print()
    print("═══ Разбор: где правы триграммы, а индекс нет ═══")
    shown = 0
    for winner, lab, r, t, l in dis_detail:
        if winner != "trgm" or shown >= 6:
            continue
        shown += 1
        print(f"  метка={lab} ранг: индекс={l or '—'} триграммы={t}")
        print(f"    A {r[pi['name_a']][:96]}")
        print(f"    B {r[pi['name_b']][:96]}")


if __name__ == "__main__":
    main()
