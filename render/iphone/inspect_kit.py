"""
inspect_kit.py — снимает «инвентарь» скачанного кита айфона, чтобы Claude знал,
как называются объекты/материалы внутри, и написал подгрузку в основную сцену.

Что делает:
  1. Ищет первый .blend в render/iphone/assets/.
  2. Открывает его.
  3. Пишет список объектов (тип, габариты, материалы), всех материалов и коллекций
     в render/iphone/out/kit_inventory.txt и в консоль.

ВАЖНО: скрипт ОТКРЫВАЕТ файл кита (заменяет текущую сцену) — это нормально,
он только для осмотра. Запускать на чистом Blender, ничего своего не потеряешь.

Запуск:
  GUI:      Scripting → Open inspect_kit.py → Run Script
  Headless: blender --background --python render/iphone/inspect_kit.py
Потом пришли мне содержимое out/kit_inventory.txt (или запушь его).
"""

import os
import glob
import bpy

# пути считаем ДО open_mainfile (после открытия data-блоки заменятся)
try:
    HERE = os.path.dirname(os.path.realpath(__file__))
except NameError:
    HERE = os.path.dirname(bpy.data.filepath) or os.getcwd()

ROOT     = os.path.normpath(os.path.join(HERE, "..", ".."))
ASSETS   = os.path.join(ROOT, "render", "iphone", "assets")
OUT_DIR  = os.path.join(ROOT, "render", "iphone", "out")
INV_PATH = os.path.join(OUT_DIR, "kit_inventory.txt")
os.makedirs(ASSETS, exist_ok=True)
os.makedirs(OUT_DIR, exist_ok=True)


def main():
    blends = sorted(glob.glob(os.path.join(ASSETS, "**", "*.blend"), recursive=True))
    if not blends:
        print(f"[inspect] !!! нет .blend в {ASSETS}")
        print("[inspect] положи туда .blend кита и запусти снова.")
        return

    kit = blends[0]
    print(f"[inspect] открываю: {kit}")
    bpy.ops.wm.open_mainfile(filepath=kit)

    lines = []
    def w(s=""):
        lines.append(s)
        print(s)

    w(f"KIT FILE: {kit}")
    w("=" * 70)

    w("\n## OBJECTS (имя | тип | габариты XYZ | материалы)")
    for o in sorted(bpy.data.objects, key=lambda x: x.name):
        dims = tuple(round(d, 3) for d in o.dimensions) if o.type == "MESH" else "-"
        mats = [s.material.name for s in o.material_slots if s.material] \
               if hasattr(o, "material_slots") else []
        w(f"  - {o.name} | {o.type} | {dims} | mats={mats}")

    w("\n## MATERIALS")
    for m in sorted(bpy.data.materials, key=lambda x: x.name):
        w(f"  - {m.name}")

    w("\n## COLLECTIONS (иерархия объектов)")
    for c in sorted(bpy.data.collections, key=lambda x: x.name):
        objs = [o.name for o in c.objects]
        w(f"  - {c.name}: {objs}")

    w("\n## SCENES / CAMERAS")
    for sc in bpy.data.scenes:
        cam = sc.camera.name if sc.camera else None
        w(f"  - scene '{sc.name}' camera={cam}")
    for cam in bpy.data.cameras:
        w(f"  - camera-data: {cam.name}")

    with open(INV_PATH, "w", encoding="utf-8") as f:
        f.write("\n".join(lines))
    print(f"\n[inspect] инвентарь сохранён: {INV_PATH}")
    print("[inspect] пришли его содержимое Claude.")

main()
