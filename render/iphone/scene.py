"""
scene.py — импорт реальной модели айфона из assets/, наш свет/камера/фон, рендер.

Что делает сам:
  1. находит модель (.glb/.gltf/.blend) в render/iphone/assets/;
  2. импортирует её;
  3. печатает инвентарь (объекты/материалы) в System Console;
  4. ставит тёмный фон + солнечный 3-точечный свет + камеру под модель;
  5. рендерит в render/iphone/out/.

От тебя: Pull → Run Script → F12 → пришли скриншот.
ВКЛЮЧИ КОНСОЛЬ: Window → Toggle System Console (там инвентарь и логи).

Перекрас в наш цвет и логотип добавлю следующим шагом, по твоему скриншоту.
"""

import os
import glob
import math
import bpy
from mathutils import Vector

# ─────────────────────────── КОНФИГ ───────────────────────────
CONFIG = {
    "res_x":   1080,
    "res_y":   1920,
    "samples": 96,          # превью; для финала поднимем
    "use_gpu": True,
}
WORLD_BG = (0.02, 0.012, 0.02, 1.0)   # тёмный berry-фон под цвет сайта

# Наш плам для корпуса (sRGB 0..1). Подбираем по скриншоту.
PLUM_SRGB = (0.37, 0.086, 0.25)       # ~#5e1640 — глубокий berry/plum
# Материалы корпуса, которые красим принудительно (вдруг автодетект промахнётся):
FORCE_BODY_MATS = {"SLmJkLdkhbbuEfG", "sJxAokqqlZYuwzy"}

# Если автопоиск не находит модель — впиши путь вручную:
MODEL_PATH = r""   # напр. r"C:\...\render\iphone\assets\...\scene.gltf"


# ─────────────────────── ПУТИ / ПОИСК ─────────────────────────
def script_dir():
    try:
        for t in bpy.data.texts:
            if t.filepath and t.filepath.lower().endswith("scene.py"):
                return os.path.dirname(bpy.path.abspath(t.filepath))
    except Exception:
        pass
    try:
        return os.path.dirname(os.path.realpath(__file__))
    except NameError:
        return None

SDIR = script_dir()
OUT_DIR = os.path.join(SDIR, "out") if SDIR else os.getcwd()
os.makedirs(OUT_DIR, exist_ok=True)


def find_model():
    if MODEL_PATH and os.path.exists(MODEL_PATH):
        return MODEL_PATH
    if not SDIR:
        return None
    assets = os.path.join(SDIR, "assets")
    found = []
    for ext in ("*.glb", "*.gltf", "*.blend"):
        found += glob.glob(os.path.join(assets, "**", ext), recursive=True)
    found = [f for f in found if "thumbnail" not in os.path.basename(f).lower()]
    print(f"[scene] модели в {assets}:")
    for f in found:
        print(f"    {os.path.getsize(f)//1024:>8} KB  {f}")
    return max(found, key=os.path.getsize) if found else None


# ───────────────────────── ИМПОРТ ─────────────────────────────
def import_model(path):
    ext = os.path.splitext(path)[1].lower()
    bpy.ops.wm.read_homefile(use_empty=True)     # чистая сцена
    if ext in (".glb", ".gltf"):
        bpy.ops.import_scene.gltf(filepath=path)
    elif ext == ".blend":
        with bpy.data.libraries.load(path) as (src, dst):
            dst.objects = list(src.objects)
        for o in dst.objects:
            if o is not None:
                bpy.context.collection.objects.link(o)
    return [o for o in bpy.context.scene.objects if o.type == "MESH"]


def srgb_to_linear(c):
    return [(v / 12.92 if v <= 0.04045 else ((v + 0.055) / 1.055) ** 2.4) for v in c]


def get_principled(mat):
    if not mat.use_nodes:
        return None
    for n in mat.node_tree.nodes:
        if n.type == "BSDF_PRINCIPLED":
            return n
    return None


def is_coppery(rgb):
    """Оранжево-медный базовый цвет: R доминирует, синего мало."""
    r, g, b = rgb[0], rgb[1], rgb[2]
    return r > 0.18 and r > g > b and (r - b) > 0.08


def recolor_to_plum():
    """Красим все оранжево-медные (и явно корпусные) материалы в наш плам.
    Печатает диагностику каждого материала — по ней добиваем точно."""
    plum = srgb_to_linear(PLUM_SRGB) + [1.0]
    print("--- MATERIAL DIAGNOSTICS (имя | base | textured | metal | rough) ---")
    changed = []
    for mat in bpy.data.materials:
        p = get_principled(mat)
        if not p:
            print(f"  {mat.name}: нет Principled BSDF")
            continue
        bc = p.inputs.get("Base Color")
        col = tuple(round(c, 3) for c in bc.default_value[:3]) if bc else None
        textured = bool(bc and bc.is_linked)
        metal = round(p.inputs["Metallic"].default_value, 2) if "Metallic" in p.inputs else "-"
        rough = round(p.inputs["Roughness"].default_value, 2) if "Roughness" in p.inputs else "-"
        print(f"  {mat.name}: base={col} tex={textured} metal={metal} rough={rough}")

        force = mat.name in FORCE_BODY_MATS
        if bc and not textured and (is_coppery(bc.default_value) or force):
            bc.default_value = plum
            changed.append(mat.name)
        elif force and textured:
            # корпус оказался текстурным — приглушим текстуру тинтом плама
            print(f"  [recolor] {mat.name}: корпус ТЕКСТУРНЫЙ — нужен тинт (добью след. шагом)")
    print(f"[recolor] перекрашены в плам: {changed}")


def print_inventory(meshes):
    print("=" * 70)
    print("=== KIT INVENTORY ===")
    for o in sorted(bpy.data.objects, key=lambda x: x.name):
        dims = tuple(round(d, 4) for d in o.dimensions) if o.type == "MESH" else "-"
        mats = [s.material.name for s in o.material_slots if s.material] \
               if hasattr(o, "material_slots") else []
        print(f"  - {o.name} | {o.type} | dims={dims} | mats={mats}")
    print("--- MATERIALS ---")
    for m in bpy.data.materials:
        print(f"  - {m.name}")
    print("=" * 70)


# ─────────────────── ГАБАРИТЫ / КАМЕРА / СВЕТ ──────────────────
def world_bounds(meshes):
    """Центр и размер общего bounding box модели в мировых координатах."""
    mn = Vector(( 1e18,  1e18,  1e18))
    mx = Vector((-1e18, -1e18, -1e18))
    for o in meshes:
        for corner in o.bound_box:
            wc = o.matrix_world @ Vector(corner)
            mn = Vector(map(min, mn, wc))
            mx = Vector(map(max, mx, wc))
    center = (mn + mx) / 2
    size = max((mx - mn).x, (mx - mn).y, (mx - mn).z) or 1.0
    return center, size


def setup_camera(center, size):
    cam_data = bpy.data.cameras.new("HeroCam")
    cam_data.lens = 85
    cam = bpy.data.objects.new("HeroCam", cam_data)
    bpy.context.collection.objects.link(cam)
    # 3/4 ракурс; какая сторона «лицо/спина» — разберём по скриншоту
    offset = Vector((0.9, -1.0, 0.35)).normalized() * size * 3.2
    cam.location = center + offset

    tgt = bpy.data.objects.new("CamTarget", None)
    tgt.location = center
    bpy.context.collection.objects.link(tgt)
    c = cam.constraints.new("TRACK_TO")
    c.target = tgt
    c.track_axis = "TRACK_NEGATIVE_Z"
    c.up_axis = "UP_Y"
    bpy.context.scene.camera = cam


def add_sun(name, rot, energy):
    la = bpy.data.lights.new(name, "SUN")     # солнце — не зависит от масштаба модели
    la.energy = energy
    obj = bpy.data.objects.new(name, la)
    obj.rotation_euler = rot
    bpy.context.collection.objects.link(obj)


def setup_lights():
    add_sun("Key",  (math.radians(55), math.radians(10), math.radians(-40)), 4.0)
    add_sun("Fill", (math.radians(70), 0,                math.radians(60)),  1.5)
    add_sun("Rim",  (math.radians(120), 0,               math.radians(150)), 3.0)


def setup_world():
    w = bpy.data.worlds.new("World")
    bpy.context.scene.world = w
    w.use_nodes = True
    bg = w.node_tree.nodes.get("Background")
    bg.inputs["Color"].default_value = WORLD_BG
    bg.inputs["Strength"].default_value = 0.3


# ──────────────────────────── РЕНДЕР ──────────────────────────
def try_enable_gpu():
    try:
        prefs = bpy.context.preferences.addons["cycles"].preferences
        for backend in ("OPTIX", "CUDA", "HIP", "METAL", "ONEAPI"):
            try:
                prefs.compute_device_type = backend
                prefs.get_devices()
                if any(d.type != "CPU" for d in prefs.devices):
                    for d in prefs.devices:
                        d.use = (d.type != "CPU")
                    bpy.context.scene.cycles.device = "GPU"
                    print(f"[scene] GPU: {backend}")
                    return
            except Exception:
                continue
        print("[scene] GPU не найден — CPU")
    except Exception as e:
        print(f"[scene] GPU init: {e}")


def setup_render():
    sc = bpy.context.scene
    sc.render.engine = "CYCLES"
    sc.cycles.samples = CONFIG["samples"]
    sc.cycles.use_denoising = True
    sc.render.resolution_x = CONFIG["res_x"]
    sc.render.resolution_y = CONFIG["res_y"]
    sc.render.image_settings.file_format = "PNG"
    sc.view_settings.view_transform = "AgX"
    if CONFIG["use_gpu"]:
        try_enable_gpu()
    out = os.path.join(OUT_DIR, "still.png")
    sc.render.filepath = out
    return out


# ──────────────────────────── MAIN ────────────────────────────
def main():
    model = find_model()
    if not model:
        print("\n[scene] !!! Модель не найдена в render/iphone/assets/")
        print("[scene] Положи туда .glb/.gltf (+текстуры) ИЛИ впиши MODEL_PATH вверху.")
        return
    print(f"[scene] импортирую: {model}")
    meshes = import_model(model)
    if not meshes:
        print("[scene] !!! модель импортировалась без мешей — проверь файл.")
        return
    print_inventory(meshes)
    recolor_to_plum()

    center, size = world_bounds(meshes)
    print(f"[scene] центр={tuple(round(c,3) for c in center)} размер={round(size,3)}")
    setup_world()
    setup_lights()
    setup_camera(center, size)
    out = setup_render()

    if bpy.app.background:
        bpy.ops.render.render(write_still=True)
        print(f"[scene] рендер сохранён: {out}")
    else:
        print(f"[scene] сцена собрана. Жми F12 — рендер уйдёт в {out}")

main()
