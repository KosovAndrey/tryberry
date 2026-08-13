#!/usr/bin/env python3
"""Насколько названия товаров несут извлекаемый код модели — по категориям.

Отвечает на вопрос из docs/PRODUCT-MATCH-JVM.md §11: с какой категории начинать
сопоставление товаров. Брать надо ту, где ТЕКСТ реально решает. Если в названиях
почти всегда есть артикул производителя, матчинг вырождается в сравнение кодов
регэкспом, и строить поверх этого поисковый индекс незачем.

Вход — TSV от cmd/title-probe: категория \t бренд \t название.

    python3 scripts/title-code-coverage.py titles.tsv
"""
import re
import sys
from collections import defaultdict

# Артикул в скобках: «JURA E8 Piano black EC (15584)» — встречен живьём на
# карточке Я.Маркета, см. PRODUCT-MATCH-JVM.md §3.
RE_PARENS = re.compile(r"\((\d{4,7})\)")

# Латинский токен с буквами И цифрами: SM-A546E, KQJHQ01ZM, MTMV3, HD-2200.
# Требуем минимум две цифры — иначе сюда попадают «iPhone 15» и подобное.
RE_TOKEN = re.compile(r"\b(?=[A-Za-z0-9-]{4,20}\b)[A-Za-z][A-Za-z0-9-]*\b")

# Короткий модельный токен: E8, S24, X3. Сигнал слабее — таких «моделей» много
# и они часто совпадают у разных производителей.
RE_SHORT = re.compile(r"\b[A-Za-z]{1,3}-?\d{1,3}\b")

# Числовой квалификатор варианта: 128 ГБ, 1.5 л, 3 шт. Это НЕ идентичность, а
# то, по чему варианты РАЗЛИЧАЮТСЯ — самый опасный тип ошибки (§9).
RE_QUALIFIER = re.compile(
    r"\b\d+([.,]\d+)?\s*(гб|тб|мб|гб?|ml|мл|л|г|кг|мм|см|дюйм|шт|w|вт|мач|mah)\b",
    re.I,
)


# Хвост неразобранной escape-последовательности (&nbsp и подобное). Ловился
# как «код модели», пока названия тянулись регэкспом по сырому JSON.
RE_ESCAPE_JUNK = re.compile(r"^u[0-9a-fA-F]{4}")


def strong_code(title: str):
    """Сильный код: артикул в скобках либо латинский токен с >=2 цифрами."""
    m = RE_PARENS.search(title)
    if m:
        return m.group(1)
    for m in RE_TOKEN.finditer(title):
        tok = m.group(0)
        if RE_ESCAPE_JUNK.match(tok):
            continue
        if sum(c.isdigit() for c in tok) >= 2 and any(c.isalpha() for c in tok):
            return tok
    return None


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)

    rows = defaultdict(list)
    with open(sys.argv[1], encoding="utf-8") as f:
        for line in f:
            parts = line.rstrip("\n").split("\t")
            if len(parts) < 3:
                continue
            rows[parts[0]].append((parts[1], parts[2]))

    print(f"{'категория':<24} {'n':>4} {'сильный код':>12} {'слабый':>8} "
          f"{'ничего':>8} {'квалиф.':>8}")
    print("-" * 70)

    totals = [0, 0, 0, 0]
    for cat, items in rows.items():
        n = len(items)
        strong = weak = none = qual = 0
        for _brand, title in items:
            if strong_code(title):
                strong += 1
            elif RE_SHORT.search(title):
                weak += 1
            else:
                none += 1
            if RE_QUALIFIER.search(title):
                qual += 1
        totals[0] += n
        totals[1] += strong
        totals[2] += weak
        totals[3] += none
        pc = lambda x: f"{100 * x / n:5.1f}%" if n else "    -"
        print(f"{cat:<24} {n:>4} {pc(strong):>12} {pc(weak):>8} "
              f"{pc(none):>8} {pc(qual):>8}")

    n = totals[0]
    if n:
        print("-" * 70)
        print(f"{'ИТОГО':<24} {n:>4} {100*totals[1]/n:11.1f}% "
              f"{100*totals[2]/n:7.1f}% {100*totals[3]/n:7.1f}%")

    print("\n=== примеры: НЕТ кода (тут решает только текст) ===")
    shown = 0
    for cat, items in rows.items():
        for _brand, title in items:
            if not strong_code(title) and not RE_SHORT.search(title):
                print(f"  [{cat}] {title[:110]}")
                shown += 1
                break
        if shown >= 8:
            break

    print("\n=== примеры: ЕСТЬ сильный код ===")
    shown = 0
    for cat, items in rows.items():
        for _brand, title in items:
            c = strong_code(title)
            if c:
                print(f"  [{cat}] {c:<14} | {title[:95]}")
                shown += 1
                break
        if shown >= 8:
            break


if __name__ == "__main__":
    main()
