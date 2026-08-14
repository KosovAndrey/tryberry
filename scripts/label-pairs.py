#!/usr/bin/env python3
"""Разметка пар «тот же товар» одним нажатием клавиши.

    python3 scripts/label-pairs.py testdata/product-match/gold-smartphones.tsv

Показывает только НЕразмеченные пары и сохраняет файл после каждого ответа —
можно бросить в любой момент и продолжить позже той же командой.

Клавиши: 1 — тот же товар, 0 — разные, s — пропустить, q — выйти.

Правила (зафиксированы до разметки, docs/PRODUCT-MATCH-JVM.md §10):
    другой цвет ................ 1  (обещание продукта — где взять дешевле)
    другой объём памяти ........ 0
    Ростест (EAC) vs Global .... 0  (гарантия и диапазоны — существенно)
    восстановленный vs новый ... 0
    Lite / Pro / Max vs базовая  0  (суффикс — часть имени модели)
    комплект vs одиночный ...... 0
"""
import sys
import termios
import tty

HELP = __doc__.split("Правила")[1]


def read_key() -> str:
    """Одна клавиша без Enter — иначе на 420 парах устанешь жать ввод.

    Если ввод не с терминала (пайп, запуск из скрипта), raw-режим недоступен —
    падать из-за этого незачем, читаем построчно.
    """
    if not sys.stdin.isatty():
        line = sys.stdin.readline()
        # Пустая строка = EOF. Раньше здесь возвращалось "q", и размётчик молча
        # выходил, будто пользователь так решил, — вместо того чтобы сказать, что
        # ввода нет. Тихий выход вместо ошибки хуже ошибки.
        if not line:
            print("\n[ввод закончился] Нечего читать: запусти в интерактивном "
                  "терминале.", file=sys.stderr)
            sys.exit(2)
        return line.strip()[:1]
    fd = sys.stdin.fileno()
    old = termios.tcgetattr(fd)
    try:
        tty.setraw(fd)
        return sys.stdin.read(1)
    finally:
        termios.tcsetattr(fd, termios.TCSADRAIN, old)


def main():
    if len(sys.argv) < 2:
        print(__doc__)
        sys.exit(2)
    path = sys.argv[1]

    # Проверяем ДО чтения файла: если терминала нет, разметка невозможна, и
    # сказать об этом надо прямо, а не выйти как ни в чём не бывало.
    if not sys.stdin.isatty() and "--stdin" not in sys.argv:
        print("Разметка требует интерактивного терминала: клавиши читаются без "
              "Enter.", file=sys.stderr)
        print("Запусти команду ОТДЕЛЬНО, не вставляя её вместе с другими "
              "строками —", file=sys.stderr)
        print("остаток вставки уходит на stdin и терминал перестаёт быть "
              "интерактивным.", file=sys.stderr)
        print("Для автоматизации есть флаг --stdin (ответы построчно).",
              file=sys.stderr)
        sys.exit(2)

    with open(path, encoding="utf-8") as f:
        header = f.readline().rstrip("\n")
        rows = [ln.rstrip("\n").split("\t") for ln in f]
    cols = {name: i for i, name in enumerate(header.split("\t"))}

    def save():
        with open(path, "w", encoding="utf-8") as f:
            f.write(header + "\n")
            for r in rows:
                f.write("\t".join(r) + "\n")

    todo = [i for i, r in enumerate(rows) if r[cols["label"]].strip() not in ("0", "1")]
    done = len(rows) - len(todo)
    print(f"\nвсего пар: {len(rows)}, размечено: {done}, осталось: {len(todo)}")
    print("Правила" + HELP)
    print("Клавиши: 1 — тот же товар, 0 — разные, s — пропустить, q — выйти\n")

    for n, i in enumerate(todo, 1):
        r = rows[i]
        mem = r[cols["mem"]]
        flags = r[cols["flags"]]
        # Подсказки не решают за человека, но экономят внимание: несовпадение
        # объёма и маркеры версии/состояния — самые частые причины «разные».
        mark = ""
        if mem == "РАЗНЫЙ":
            mark += "  ⚠ ПАМЯТЬ РАЗНАЯ"
        if flags.strip():
            mark += f"  ⚑ {flags.strip()}"

        print(f"── {n}/{len(todo)} (осталось {len(todo) - n + 1}) "
              f"trgm={r[cols['sim']]} память={mem}{mark}")
        print(f"   A ({r[cols['mp_a']]:<14} {r[cols['price_a']]:>10} ₽) {r[cols['name_a']][:110]}")
        print(f"   B ({r[cols['mp_b']]:<14} {r[cols['price_b']]:>10} ₽) {r[cols['name_b']][:110]}")

        while True:
            k = read_key().lower()
            if k in ("1", "0"):
                r[cols["label"]] = k
                save()
                print(f"   → {k}\n")
                break
            if k == "s":
                print("   → пропущено\n")
                break
            if k in ("q", "\x03"):  # q или Ctrl+C
                save()
                left = sum(1 for x in rows if x[cols["label"]].strip() not in ("0", "1"))
                print(f"\nсохранено. осталось неразмеченных: {left}")
                return
            print("   ? нажми 1, 0, s или q")

    save()
    labeled = sum(1 for r in rows if r[cols["label"]].strip() in ("0", "1"))
    pos = sum(1 for r in rows if r[cols["label"]].strip() == "1")
    print(f"\nготово. размечено: {labeled} из {len(rows)}, положительных: {pos}")
    print("Дальше: python3 scripts/trgm-baseline.py " + path)


if __name__ == "__main__":
    main()
