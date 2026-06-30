"""
inspect_kit.py — снимает «инвентарь» кита айфона (имена объектов/материалов/
коллекций), чтобы Claude написал подгрузку реальной модели в основную сцену.

Запуск в Blender: Scripting → Open inspect_kit.py → Run Script.
ВКЛЮЧИ КОНСОЛЬ: Window → Toggle System Console — там будет весь вывод,
даже если файл потеряется.

Если автопоиск не находит кит — впиши путь вручную в KIT_PATH ниже.
"""

import os
import glob
import bpy

# ─────────────────────────────────────────────────────────────
# ВАРИАНТ «наверняка»: впиши сюда полный путь к модели и не парься.
# Пример: r"C:\Users\Andrey\tryberrybot\render\iphone\assets\cleaned.blend"
KIT_PATH = r""
# ─────────────────────────────────────────────────────────────


def script_dir():
    """Папка этого скрипта. В Text Editor __file__ нет — берём из text-блока."""
    try:
        for t in bpy.data.texts:
            if t.filepath and t.filepath.lower().endswith("inspect_kit.py"):
                return os.path.dirname(bpy.path.abspath(t.filepath))
    except Exception:
        pass
    try:
        return os.path.dirname(os.path.realpath(__file__))   # headless-режим
    except NameError:
        return None


def resolve_kit(sdir):
    """Находим модель: ручной KIT_PATH → автопоиск .blend/.glb/.gltf в assets/."""
    if KIT_PATH and os.path.exists(KIT_PATH):
        return KIT_PATH
    if KIT_PATH:
        print(f"[inspect] !!! KIT_PATH задан, но файла нет: {KIT_PATH}")
    if not sdir:
        return None
    assets = os.path.join(sdir, "assets")
    found = []
    for ext in ("*.blend", "*.glb", "*.gltf"):
        found += glob.glob(os.path.join(assets, "**", ext), recursive=True)
    print(f"[inspect] ищу модель в: {assets}")
    for b in sorted(found):
        print(f"    {os.path.getsize(b)//1024:>8} KB  {b}")
    if not found:
        return None
    real = [b for b in found if "thumbnail" not in os.path.basename(b).lower()]
    cand = real or found
    return max(cand, key=os.path.getsize)        # модель обычно самая тяжёлая


def main():
    sdir = script_dir()
    kit = resolve_kit(sdir)

    if not kit:
        print("\n[inspect] !!! Модель не найдена.")
        print("[inspect] Положи .blend/.glb/.gltf (+ текстуры) в render/iphone/assets/")
        print("[inspect] ЛИБО впиши полный путь в KIT_PATH вверху скрипта.")
        if sdir:
            print(f"[inspect] (искал относительно: {sdir})")
        return

    # путь под инвентарь считаем ДО открытия кита (open_mainfile сотрёт data-блоки)
    out_path = os.path.join(sdir, "kit_inventory.txt") if sdir \
        else os.path.join(os.path.dirname(kit), "kit_inventory.txt")

    ext = os.path.splitext(kit)[1].lower()
    if ext == ".blend":
        print(f"[inspect] открываю .blend: {kit}")
        bpy.ops.wm.open_mainfile(filepath=kit)
    else:                                            # .glb / .gltf — импорт в чистую сцену
        print(f"[inspect] импортирую {ext}: {kit}")
        bpy.ops.wm.read_homefile(use_empty=True)     # пустая сцена без дефолтного куба
        bpy.ops.import_scene.gltf(filepath=kit)

    lines = []
    def w(s=""):
        lines.append(s)
        print(s)

    w("=" * 70)
    w("=== KIT INVENTORY (скопируй весь блок ниже и пришли Claude) ===")
    w("=" * 70)
    w(f"KIT FILE: {kit}")

    w("\n## OBJECTS (имя | тип | габариты XYZ | материалы)")
    for o in sorted(bpy.data.objects, key=lambda x: x.name):
        dims = tuple(round(d, 3) for d in o.dimensions) if o.type == "MESH" else "-"
        mats = [s.material.name for s in o.material_slots if s.material] \
               if hasattr(o, "material_slots") else []
        w(f"  - {o.name} | {o.type} | {dims} | mats={mats}")

    w("\n## MATERIALS")
    for m in sorted(bpy.data.materials, key=lambda x: x.name):
        w(f"  - {m.name}")

    w("\n## COLLECTIONS")
    for c in sorted(bpy.data.collections, key=lambda x: x.name):
        w(f"  - {c.name}: {[o.name for o in c.objects]}")

    w("\n## CAMERAS / LIGHTS")
    for cam in bpy.data.cameras:
        w(f"  - camera: {cam.name}")
    for la in bpy.data.lights:
        w(f"  - light: {la.name} ({la.type})")
    w("=" * 70)

    try:
        with open(out_path, "w", encoding="utf-8") as f:
            f.write("\n".join(lines))
        print(f"\n[inspect] сохранено в файл: {out_path}")
    except Exception as e:
        print(f"\n[inspect] файл записать не вышло ({e}), но текст выше в консоли — копируй оттуда.")

    print("[inspect] Готово. Пришли Claude блок KIT INVENTORY (из файла или консоли).")


main()
