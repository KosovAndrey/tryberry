"""
scene.py — импорт реальной модели айфона из assets/, наш свет/камера/фон, рендер.

Что делает сам:
  1. находит модель (.glb/.gltf/.blend) в render/iphone/assets/;
  2. импортирует её;
  3. печатает инвентарь (объекты/материалы) в System Console;
  4. ставит тёмный фон + солнечный 3-точечный свет + камеру под модель;
  5. рендерит в render/iphone/out/.

От тебя: Pull → Run Script → дождись двух рендеров → пришли out/still_front.png
и out/still_back.png. (Run сам рендерит оба вида, F12 не нужен.)
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
# Материалы корпуса/кнопок/яблока — красим принудительно (по отчёту materials.txt):
FORCE_BODY_MATS = {
    "SLmJkLdkhbbuEfG",   # рамка/корпус
    "sJxAokqqlZYuwzy",   # боковая/задняя
    "SMUhrjUPCjJkPUK",   # задняя панель
    "HETovHCBsEjcSiP",   # планка
    "YQFhPSFSryEqJMp",   # кнопки + планка
    "iAKEWdNafBldSCV",   # боковая кнопка
    "PJgHvfOhNXkxvzq",   # боковая кнопка
    "ieDmCkHnOnSIOcm",   # action-кнопка (обводка)
    "hiWLrxfkqoGxruc",   # action-кнопка (обводка)
}
# Чёрные: dynamic island (спереди) + яблоко (сзади, заменим на наш логотип):
BLACK_MATS = {"yPEFElLJTRhfWfw", "awYxKfiOpRgQIxD"}
# Только передний дисплей не трогаем (заменим UI отдельным шагом):
SCREEN_MATS = {"BsXHDwLKqtDOfrW"}

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


def is_warm(rgb):
    """Тёплый цвет (жёлтый/оранжевый/медный): синий — самый малый канал.
    Плюс ловим артефакт чтения анодир. оранжа (1.0, ~0.3, 1.0) — магента."""
    r, g, b = rgb[0], rgb[1], rgb[2]
    if r > 0.85 and b > 0.85 and g < 0.6:        # магента-артефакт = оранж в текстуре
        return True
    return r > 0.12 and b <= g and b <= r and (r - b) > 0.06


def base_image(p, mat):
    """Image Texture со входа Base Color; если не нашли — любая в материале."""
    bc = p.inputs.get("Base Color")
    if bc and bc.is_linked:
        seen, stack = set(), [l.from_node for l in bc.links]
        while stack:
            n = stack.pop()
            if n in seen:
                continue
            seen.add(n)
            if n.type == "TEX_IMAGE" and n.image:
                return n.image
            for inp in n.inputs:
                for l in inp.links:
                    stack.append(l.from_node)
    for n in mat.node_tree.nodes:               # фолбэк: любая картинка в материале
        if n.type == "TEX_IMAGE" and n.image:
            return n.image
    return None


def avg_image_color(img):
    """Средний цвет текстуры. numpy → если упадёт, чистый Python-фолбэк."""
    name = getattr(img, "name", "?")
    try:
        n = len(img.pixels)
    except Exception as e:
        print(f"  [avg] {name}: pixels недоступны ({e})")
        return None
    if not n or n % 4:
        print(f"  [avg] {name}: пустые/битые pixels (len={n})")
        return None
    try:
        import numpy as np
        a = np.empty(n, dtype=np.float32)
        img.pixels.foreach_get(a)
        a = a.reshape(-1, 4)
        step = max(1, a.shape[0] // 4000)
        s = a[::step, :3].mean(axis=0)
        return (float(s[0]), float(s[1]), float(s[2]))
    except Exception as e:
        print(f"  [avg] {name}: numpy не сработал ({e}), пробую Python")
    try:
        px = img.pixels[:]                       # копия (может быть медленно)
        npx = len(px) // 4
        step = max(1, npx // 2000)
        rs = gs = bs = c = 0.0
        for i in range(0, npx, step):
            rs += px[i*4]; gs += px[i*4+1]; bs += px[i*4+2]; c += 1
        return (rs/c, gs/c, bs/c) if c else None
    except Exception as e:
        print(f"  [avg] {name}: Python-фолбэк тоже упал ({e})")
        return None


def effective_base(p, mat):
    """Эффективный базовый цвет: плоский, либо средний по текстуре."""
    bc = p.inputs.get("Base Color")
    if not bc:
        return None
    if not bc.is_linked:
        return tuple(bc.default_value[:3])
    img = base_image(p, mat)
    return avg_image_color(img) if img else None


def unlink_base_color(mat, bc):
    """Снять любую текстуру/ноду с входа Base Color — останется плоский цвет."""
    nt = mat.node_tree
    for link in list(nt.links):
        if link.to_socket == bc:
            nt.links.remove(link)


def recolor_to_plum():
    """Корпусные/тёплые материалы → плоский плам. Отчёт по каждому материалу
    (цвет, тёплый ли, перекрашен ли, какие меши используют) пишем в out/materials.txt."""
    plum = srgb_to_linear(PLUM_SRGB) + [1.0]

    usage = {}                                   # материал -> [(меш, габариты)]
    for o in bpy.data.objects:
        if o.type != "MESH":
            continue
        for s in o.material_slots:
            if s.material:
                usage.setdefault(s.material.name, []).append(
                    (o.name, tuple(round(d, 3) for d in o.dimensions)))

    lines = ["--- MATERIAL DIAGNOSTICS ---"]
    changed = []
    for mat in bpy.data.materials:
        p = get_principled(mat)
        if not p:
            lines.append(f"{mat.name}: нет Principled BSDF")
            continue
        bc = p.inputs.get("Base Color")
        textured = bool(bc and bc.is_linked)
        eff = effective_base(p, mat)             # плоский ИЛИ средний по текстуре
        effr = tuple(round(c, 3) for c in eff) if eff else None
        metal = round(p.inputs["Metallic"].default_value, 2) if "Metallic" in p.inputs else "-"
        warm = bool(eff and is_warm(eff))
        is_black = mat.name in BLACK_MATS
        if mat.name in SCREEN_MATS:
            target = None
        elif is_black:
            target = [0.0, 0.0, 0.0, 1.0]
        elif mat.name in FORCE_BODY_MATS or warm:
            target = plum
        else:
            target = None
        do = target is not None

        if do:
            unlink_base_color(mat, bc)
            bc.default_value = target
            if "Metallic" in p.inputs:
                if is_black:
                    p.inputs["Metallic"].default_value = 0.0
                elif p.inputs["Metallic"].default_value < 0.5:
                    p.inputs["Metallic"].default_value = 0.85
            if "Roughness" in p.inputs:
                if is_black:
                    p.inputs["Roughness"].default_value = 0.15     # глянцевое чёрное стекло
                else:
                    r = p.inputs["Roughness"].default_value or 0.3
                    p.inputs["Roughness"].default_value = min(0.45, max(0.15, r))
            # гасим оранжевое свечение, если цвет шёл из emission
            em = p.inputs.get("Emission Color") or p.inputs.get("Emission")
            if em:
                for link in list(mat.node_tree.links):
                    if link.to_socket == em:
                        mat.node_tree.links.remove(link)
                em.default_value = (0.0, 0.0, 0.0, 1.0)
            if "Emission Strength" in p.inputs:
                p.inputs["Emission Strength"].default_value = 0.0
            changed.append(mat.name)

        lines.append(f"{mat.name}: eff={effr} tex={textured} metal={metal} "
                     f"warm={warm} recolored={'YES' if do else 'no'}")
        for mname, dims in usage.get(mat.name, [])[:6]:
            lines.append(f"    used: {mname} {dims}")

    lines.append(f"[recolor] перекрашены: {changed}")
    report = "\n".join(lines)
    print(report)
    try:
        path = os.path.join(OUT_DIR, "materials.txt")
        with open(path, "w", encoding="utf-8") as f:
            f.write(report)
        print(f"[recolor] отчёт по материалам: {path}")
    except Exception as e:
        print(f"[recolor] не записал materials.txt: {e}")


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


def create_camera():
    cam_data = bpy.data.cameras.new("HeroCam")
    cam_data.lens = 85
    cam = bpy.data.objects.new("HeroCam", cam_data)
    bpy.context.collection.objects.link(cam)
    tgt = bpy.data.objects.new("CamTarget", None)
    bpy.context.collection.objects.link(tgt)
    c = cam.constraints.new("TRACK_TO")
    c.target = tgt
    c.track_axis = "TRACK_NEGATIVE_Z"
    c.up_axis = "UP_Y"
    bpy.context.scene.camera = cam
    return cam, tgt


def place_camera(cam, tgt, center, size, view):
    # front — как на первом рендере; back — противоположная сторона (видно крышку)
    d = Vector((0.9, -1.0, 0.35)) if view == "front" else Vector((-0.9, 1.0, 0.35))
    cam.location = center + d.normalized() * size * 3.2
    tgt.location = center


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
    setup_render()
    cam, tgt = create_camera()

    # рендерим сразу оба вида — front и back — за один Run
    sc = bpy.context.scene
    for view in ("front", "back"):
        place_camera(cam, tgt, center, size, view)
        out = os.path.join(OUT_DIR, f"still_{view}.png")
        sc.render.filepath = out
        print(f"[scene] рендерю {view}…")
        bpy.ops.render.render(write_still=True)
        print(f"[scene]   готово: {out}")
    print("[scene] оба вида готовы: out/still_front.png и out/still_back.png")

main()
