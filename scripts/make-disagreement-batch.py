#!/usr/bin/env python3
"""Отбирает пары, где триграммы и индекс РАСХОДЯТСЯ, — только их и размечать.

    # собрать батч
    python3 scripts/make-disagreement-batch.py \\
        testdata/product-match/items-smartphones.tsv \\
        testdata/product-match/gold-smartphones.tsv \\
        /tmp/lucene-scored.tsv \\
        testdata/product-match/disagreement-smartphones.tsv

    # вернуть метки в основной набор
    python3 scripts/make-disagreement-batch.py --merge \\
        testdata/product-match/gold-smartphones.tsv \\
        testdata/product-match/disagreement-smartphones.tsv

Зачем. Размечать все 420 пар — часы работы, и большая их часть уходит на случаи,
где оба метода и так согласны: их метки ничего не различают. Решающее сравнение
живёт там, где методы дают РАЗНЫЙ ответ, — и таких пар в разы меньше.

ЧЕСТНАЯ ОГОВОРКА, без неё числа переоценят. На батче расхождений считается
только ОТНОСИТЕЛЬНОЕ сравнение: кто прав чаще там, где методы спорят. Абсолютные
precision и recall по нему считать НЕЛЬЗЯ — выборка смещена по построению.
Поэтому сюда же добавляется случайная контрольная группа из пар, где методы
согласны: она нужна, чтобы проверить, что согласие вообще заслуживает доверия.

Расхождение определяется БЕЗ порогов, которых у нас пока нет: сравниваются
РАНГИ. Пара считается спорной, если один метод ставит её в тройку лучших
кандидатов, а другой не ставит.
"""
import random
import sys
from collections import defaultdict
from importlib.machinery import SourceFileLoader

_base = __file__.rsplit("/", 1)[0]
trgm = SourceFileLoader("trgm", _base + "/trgm-baseline.py").load_module()
mp = SourceFileLoader("mp", _base + "/make-pairs.py").load_module()

TOP_N = 3        # «нашёл» = поставил в тройку
# Сколько расхождений брать. Всех 158 не нужно: чтобы увидеть, кто прав чаще,
# хватает нескольких десятков, а разметка стоит человеко-часов. Берём поровну с
# ОБЕИХ сторон спора — иначе проверим только одно направление, — и начинаем с
# самых показательных: где один метод ставит пару первой, а другой не находит.
PER_SIDE = 40
CONTROL = 30     # размер контрольной группы согласия
SEED = 20260814


def load_tsv(path):
    with open(path, encoding="utf-8") as f:
        header = f.readline().rstrip("\n").split("\t")
        rows = [ln.rstrip("\n").split("\t") for ln in f]
    return header, [r for r in rows if len(r) == len(header)]


def merge(gold_path, batch_path):
    ghead, grows = load_tsv(gold_path)
    bhead, brows = load_tsv(batch_path)
    gi = {n: i for i, n in enumerate(ghead)}
    bi = {n: i for i, n in enumerate(bhead)}
    labels = {r[bi["id"]]: r[bi["label"]].strip() for r in brows
              if r[bi["label"]].strip() in ("0", "1")}
    n = 0
    for r in grows:
        lab = labels.get(r[gi["id"]])
        if lab and r[gi["label"]].strip() not in ("0", "1"):
            r[gi["label"]] = lab
            n += 1
    with open(gold_path, "w", encoding="utf-8") as f:
        f.write("\t".join(ghead) + "\n")
        for r in grows:
            f.write("\t".join(r) + "\n")
    total = sum(1 for r in grows if r[gi["label"]].strip() in ("0", "1"))
    print(f"перенесено меток: {n}; всего размечено в наборе: {total} из {len(grows)}")


def main():
    if "--merge" in sys.argv:
        args = [a for a in sys.argv[1:] if a != "--merge"]
        merge(args[0], args[1])
        return
    if len(sys.argv) < 5:
        print(__doc__)
        sys.exit(2)
    items_path, gold_path, scored_path, out_path = sys.argv[1:5]

    ihead, irows = load_tsv(items_path)
    ii = {n: i for i, n in enumerate(ihead)}
    items = [dict(zip(ihead, r)) for r in irows]
    by_url = {it["url"]: it for it in items}
    for it in items:
        it["_brand"] = mp.brand_of(it)

    # Кандидаты по бренду — то же блокирование, что у индекса: сравниваем методы,
    # а не их предпочтения по отбору бренда.
    by_brand = defaultdict(list)
    for it in items:
        by_brand[it["_brand"]].append(it)

    ghead, grows = load_tsv(gold_path)
    gi = {n: i for i, n in enumerate(ghead)}

    shead, srows = load_tsv(scored_path)
    si = {n: i for i, n in enumerate(shead)}
    lucene_rank = {r[si["id"]]: int(r[si["lucene_rank"]]) for r in srows}

    def trgm_rank(a, b):
        """Место b среди кандидатов a по триграммной схожести (0 — вне топа)."""
        cands = [(trgm.similarity(a["name"], c["name"]), c["url"])
                 for c in by_brand[a["_brand"]]
                 if c["marketplace"] != a["marketplace"]]
        cands.sort(key=lambda x: -x[0])
        for pos, (_s, url) in enumerate(cands[:TOP_N], 1):
            if url == b["url"]:
                return pos
        return 0

    disagree, agree = [], []
    for r in grows:
        a, b = by_url.get(r[gi["url_a"]]), by_url.get(r[gi["url_b"]])
        if not a or not b:
            continue
        # Ранг считаем в обе стороны: пара может находиться только в одну.
        t = min([x for x in (trgm_rank(a, b), trgm_rank(b, a)) if x] or [0])
        l = lucene_rank.get(r[gi["id"]], 0)
        t_top = 0 < t <= TOP_N
        l_top = 0 < l <= TOP_N
        (disagree if t_top != l_top else agree).append((r, t, l))

    # Показательность: чем выше ранг у нашедшего метода, тем чище свидетельство.
    only_lucene = sorted((x for x in disagree if 0 < x[2] <= TOP_N), key=lambda x: x[2])
    only_trgm = sorted((x for x in disagree if 0 < x[1] <= TOP_N), key=lambda x: x[1])
    picked = only_lucene[:PER_SIDE] + only_trgm[:PER_SIDE]

    random.seed(SEED)
    control = random.sample(agree, min(CONTROL, len(agree)))
    batch = picked + control
    batch.sort(key=lambda x: x[0][gi["id"]])

    with open(out_path, "w", encoding="utf-8") as f:
        f.write("\t".join(ghead) + "\n")
        for r, _t, _l in batch:
            f.write("\t".join(r) + "\n")

    only_l = sum(1 for _r, t, l in disagree if 0 < l <= TOP_N)
    only_t = len(disagree) - only_l
    unlabeled = sum(1 for r, _t, _l in batch if r[gi["label"]].strip() not in ("0", "1"))
    print(f"пар всего: {len(grows)}")
    print(f"расхождений: {len(disagree)} "
          f"(только индекс нашёл: {only_l}, только триграммы: {only_t})")
    print(f"взято в батч: {len(only_lucene[:PER_SIDE])} + {len(only_trgm[:PER_SIDE])} "
          f"(поровну с обеих сторон спора)")
    print(f"контрольная группа согласия: {len(control)}")
    print(f"в батч на разметку: {len(batch)}, из них неразмеченных: {unlabeled}")
    print(f"→ {out_path}")


if __name__ == "__main__":
    main()
