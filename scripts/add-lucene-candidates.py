#!/usr/bin/env python3
"""Дополняет золотой набор кандидатами, которых предложил ИНДЕКС, а не триграммы.

    python3 scripts/add-lucene-candidates.py \\
        testdata/product-match/items-smartphones.tsv \\
        testdata/product-match/gold-smartphones.tsv \\
        /tmp/lucene-proposals.tsv

Зачем. Набор от scripts/make-pairs.py отобрало ТРИГРАММНОЕ сито: блокинг по
бренду плюс верхушка по Жаккару. Значит он структурно не способен показать
случаи, где индекс находит то, что триграммы пропустили, — а это главное
преимущество Lucene, ради которого всё и затевалось («iPhone15» против
«iPhone 15» даёт триграммам 0.40 и уходит из выборки). Разметив только его,
измеришь ТОЧНОСТЬ на чужих кандидатах и не измеришь ПОЛНОТУ вовсе.

Поэтому сюда берутся ровно те предложения индекса, которые триграммы оценили
НИЗКО (ниже порога, с которым работал make-pairs) — там и живёт разница между
методами. Пары, где обе стороны согласны, добавлять бессмысленно: они уже есть.

Результат дописывается в конец файла набора в том же формате, с пустым label.
"""
import sys

sys.path.insert(0, __file__.rsplit("/", 1)[0])
from importlib.machinery import SourceFileLoader  # noqa: E402

_base = __file__.rsplit("/", 1)[0]
trgm = SourceFileLoader("trgm", _base + "/trgm-baseline.py").load_module()
mp = SourceFileLoader("mp", _base + "/make-pairs.py").load_module()

# Порог, ниже которого триграммы пару НЕ предложили бы (дефолт --min-sim
# в make-pairs.py). Только такие и интересны.
TRGM_CUTOFF = 0.25
# Сколько добавить. Разметка дорогая: берём верхушку по оценке индекса, где
# вероятность настоящих пар выше, а не случайный хвост.
LIMIT = 120


def main():
    if len(sys.argv) < 4:
        print(__doc__)
        sys.exit(2)
    items_path, gold_path, prop_path = sys.argv[1:4]

    items = {}
    with open(items_path, encoding="utf-8") as f:
        header = f.readline().rstrip("\n").split("\t")
        for line in f:
            p = line.rstrip("\n").split("\t")
            if len(p) == len(header):
                items[p[5]] = dict(zip(header, p))

    with open(gold_path, encoding="utf-8") as f:
        gold_header = f.readline().rstrip("\n")
        gold_lines = [ln.rstrip("\n") for ln in f]
    next_id = len(gold_lines) + 1

    cand = []
    with open(prop_path, encoding="utf-8") as f:
        f.readline()
        for line in f:
            p = line.rstrip("\n").split("\t")
            if len(p) < 4:
                continue
            a, b = items.get(p[0]), items.get(p[1])
            if not a or not b:
                continue
            s = trgm.similarity(a["name"], b["name"])
            if s >= TRGM_CUTOFF:
                continue  # триграммы и так предложили бы — не интересно
            cand.append((float(p[2]), s, a, b))

    cand.sort(key=lambda x: -x[0])
    picked = cand[:LIMIT]

    added = []
    for lucene_score, s, a, b in picked:
        mem = mp.mem_verdict(mp.storage_of(a["name"]), mp.storage_of(b["name"]))
        flags = "; ".join(f"A[{x}]" for x in mp.markers_of(a["name"])) + \
                " " + "; ".join(f"B[{x}]" for x in mp.markers_of(b["name"]))
        added.append("\t".join([
            "", str(next_id + len(added)), f"{s:.3f}", mem, flags.strip(),
            mp.brand_of({"brand": a["brand"], "name": a["name"]}),
            a["marketplace"], a["name"], a["price"],
            b["marketplace"], b["name"], b["price"], a["url"], b["url"],
        ]))

    with open(gold_path, "w", encoding="utf-8") as f:
        f.write(gold_header + "\n")
        for ln in gold_lines:
            f.write(ln + "\n")
        for ln in added:
            f.write(ln + "\n")

    print(f"кандидатов от индекса: {len(cand)} (с низкой триграммной схожестью)")
    print(f"добавлено в набор: {len(added)} → всего пар {len(gold_lines) + len(added)}")
    print()
    print("Это те пары, которые триграммы НЕ предложили бы. Если среди них после")
    print("разметки окажется много настоящих — вот и измеренное преимущество")
    print("индекса. Если почти все ложные — преимущества нет, и это тоже ответ.")


if __name__ == "__main__":
    main()
