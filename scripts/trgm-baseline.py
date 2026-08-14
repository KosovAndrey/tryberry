#!/usr/bin/env python3
"""Триграммный baseline сопоставления товаров — КОНТРОЛЬНАЯ ГРУППА для Lucene.

Вход — размеченный набор от scripts/make-pairs.py (колонка label: 1/0).

    python3 scripts/trgm-baseline.py testdata/product-match/gold-smartphones.tsv

Зачем. В docs/PRODUCT-MATCH-JVM.md §10 заранее записано условие отказа: если
Lucene не обгонит честный pg_trgm — сервис не пишем. Без этого числа фраза
«Lucene даёт precision 0.95» не значит ничего: может, Postgres даёт 0.94 двумя
десятками строк SQL, и второй рантайм не оправдан.

Почему на Python, а не в Postgres. Алгоритм pg_trgm воспроизведён дословно
(нижний регистр, разбиение на слова, каждое слово дополняется двумя пробелами
слева и одним справа, similarity = |пересечение| / |объединение| множеств
триграмм). Так baseline считается где угодно и не требует ставить расширение на
прод — это была бы правка боевой БД ради эксперимента. Сверить с оригиналом
можно одной строкой:
    SELECT similarity('смартфон xiaomi', 'xiaomi смартфон');

ЧЕСТНАЯ ОГОВОРКА про метрики. Набор смещён по построению: в него отбирались
кандидаты с высокой схожестью и квота трудных негативов. Поэтому здесь честно
считаются только:
  - precision при пороге (доля верных среди выданных),
  - recall СРЕДИ РАЗМЕЧЕННЫХ положительных.
Абсолютное coverage по каталогу так измерить НЕЛЬЗЯ — для него нужна случайная
выборка товаров, а не выборка кандидатов. Не путать одно с другим при сравнении
с Lucene: сравнивать надо на ЭТОМ ЖЕ наборе и по этим же двум метрикам.
"""
import sys
from collections import Counter

NONWORD = set(" \t\n\r,.;:!?()[]{}/\\|\"'«»—–-+*&%#@~<>=_")


def trigrams(text: str) -> set:
    """Множество триграмм по правилам pg_trgm."""
    out = set()
    word = []
    words = []
    for ch in text.lower():
        if ch in NONWORD:
            if word:
                words.append("".join(word))
                word = []
        else:
            word.append(ch)
    if word:
        words.append("".join(word))

    for w in words:
        padded = "  " + w + " "
        for i in range(len(padded) - 2):
            out.add(padded[i:i + 3])
    return out


def similarity(a: str, b: str) -> float:
    ta, tb = trigrams(a), trigrams(b)
    if not ta or not tb:
        return 0.0
    return len(ta & tb) / len(ta | tb)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)

    rows = []
    with open(sys.argv[1], encoding="utf-8") as f:
        header = f.readline().rstrip("\n").split("\t")
        idx = {name: i for i, name in enumerate(header)}
        for line in f:
            p = line.rstrip("\n").split("\t")
            if len(p) != len(header):
                continue
            label = p[idx["label"]].strip()
            if label not in ("0", "1"):
                continue  # неразмеченное пропускаем молча
            rows.append({
                "y": int(label),
                "a": p[idx["name_a"]],
                "b": p[idx["name_b"]],
                "mem": p[idx["mem"]],
            })

    if not rows:
        print("В файле нет размеченных строк (колонка label пуста).", file=sys.stderr)
        print("Разметка: 1 — тот же товар, 0 — разные. Правила — §10 дока.", file=sys.stderr)
        sys.exit(1)

    for r in rows:
        r["s"] = similarity(r["a"], r["b"])

    pos = sum(r["y"] for r in rows)
    print(f"размечено пар: {len(rows)}, положительных: {pos}, "
          f"отрицательных: {len(rows) - pos}")
    hard = [r for r in rows if r["mem"] == "РАЗНЫЙ"]
    print(f"из них трудных негативов (разный объём памяти): {len(hard)}, "
          f"положительных среди них: {sum(r['y'] for r in hard)}")
    print()

    print(f"{'порог':>6} {'выдано':>7} {'верных':>7} {'precision':>10} "
          f"{'recall':>8} {'F1':>7} {'ошибок на трудных':>18}")
    print("-" * 70)

    best = None
    for i in range(4, 20):
        t = i / 20.0
        pred = [r for r in rows if r["s"] >= t]
        tp = sum(r["y"] for r in pred)
        prec = tp / len(pred) if pred else 0.0
        rec = tp / pos if pos else 0.0
        f1 = 2 * prec * rec / (prec + rec) if prec + rec else 0.0
        # Ошибка на трудном негативе — выданная пара, которая на самом деле разные
        # товары, отличающиеся объёмом памяти. Самый дорогой тип ошибки (§9).
        hard_err = sum(1 for r in pred if r["y"] == 0 and r["mem"] == "РАЗНЫЙ")
        print(f"{t:>6.2f} {len(pred):>7} {tp:>7} {prec:>10.3f} "
              f"{rec:>8.3f} {f1:>7.3f} {hard_err:>18}")
        # Планка продукта — precision >= 0.95 (§10). Среди проходящих её берём
        # порог с максимальным recall.
        if prec >= 0.95 and (best is None or rec > best[2]):
            best = (t, prec, rec, hard_err)

    print()
    if best:
        t, prec, rec, hard_err = best
        print(f"Планка precision >= 0.95 достигается при пороге {t:.2f}: "
              f"precision {prec:.3f}, recall {rec:.3f}, "
              f"ошибок на трудных негативах {hard_err}.")
    else:
        print("Планка precision >= 0.95 НЕ достигается ни при каком пороге —")
        print("голых триграмм не хватает. Это и есть аргумент в пользу индекса,")
        print("но проверять его надо тем же набором и теми же метриками.")

    print()
    print("=== Где триграммы ошибаются: 10 худших выданных пар при пороге 0.5 ===")
    wrong = sorted((r for r in rows if r["s"] >= 0.5 and r["y"] == 0),
                   key=lambda r: -r["s"])[:10]
    for r in wrong:
        print(f"  sim={r['s']:.3f} память={r['mem']}")
        print(f"    A {r['a'][:100]}")
        print(f"    B {r['b'][:100]}")

    print()
    print("=== Что триграммы пропустили: 10 положительных с низкой схожестью ===")
    missed = sorted((r for r in rows if r["y"] == 1), key=lambda r: r["s"])[:10]
    for r in missed:
        print(f"  sim={r['s']:.3f}")
        print(f"    A {r['a'][:100]}")
        print(f"    B {r['b'][:100]}")


if __name__ == "__main__":
    main()
