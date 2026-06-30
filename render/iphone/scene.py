"""
scene.py — iteration 1: процедурный плейсхолдер-телефон для проверки всего
рендер-пайплайна (геометрия → материалы → логотип-текстура → свет → камера →
Cycles-рендер). НЕ финальная модель айфона — её подменим следующим шагом.

Задача итерации: убедиться, что цепочка собирается и логотип tone-on-tone
ложится на крышку. Премиальность света/материалов доводим по скриншотам.

Запуск (см. README.md):
  GUI:      Scripting → открыть scene.py → Run Script
  Headless: blender --background --python render/iphone/scene.py
"""

import os
import math
import bpy

# ─────────────────────────── КОНФИГ ───────────────────────────
CONFIG = {
    "view":        "back",      # "back" (видим крышку с логотипом) или "front"
    "res_x":       1080,
    "res_y":       1920,        # 9:16 — годится и под hero, и под Reels
    "samples":     128,         # превью; для финала поднять до 512
    "use_gpu":     True,        # пытаемся OptiX/CUDA, иначе откат на CPU
    "out_name":    "still",     # имя файла рендера (без расширения)
}

# Бренд-палитра (из web/index.html :root). Тёмно-фиолетовый корпус под цвет сайта.
PURPLE_BODY   = (0.14, 0.05, 0.13, 1.0)   # тёмный plum-металл
PURPLE_LOGO   = (0.30, 0.12, 0.26, 1.0)   # tone-on-tone, чуть светлее — ловит блик
WORLD_BG      = (0.02, 0.01, 0.02, 1.0)   # почти чёрный berry-фон

# Пропорции iPhone (мм → дециметры Blender): 71.5 × 146.7 × 7.8
PW, PH, PD = 0.715, 1.467, 0.078


# ─────────────────────── ПУТИ К АССЕТАМ ───────────────────────
def repo_root():
    """render/iphone/scene.py → корень репо (два уровня вверх)."""
    try:
        here = os.path.dirname(os.path.realpath(__file__))
    except NameError:                      # запуск из Text Editor без __file__
        here = os.path.dirname(bpy.data.filepath) or os.getcwd()
    return os.path.normpath(os.path.join(here, "..", ".."))

ROOT      = repo_root()
LOGO_PATH = os.path.join(ROOT, "web", "logo.png")
OUT_DIR   = os.path.join(ROOT, "render", "iphone", "out")
os.makedirs(OUT_DIR, exist_ok=True)


# ─────────────────────────── УТИЛИТЫ ──────────────────────────
def clear_scene():
    bpy.ops.object.select_all(action="SELECT")
    bpy.ops.object.delete(use_global=False)
    for coll in (bpy.data.meshes, bpy.data.materials, bpy.data.lights,
                 bpy.data.images, bpy.data.cameras):
        for block in list(coll):
            if block.users == 0:
                coll.remove(block)

def set_input(node, names, value):
    """Принципиальный BSDF менял имена входов между версиями — пробуем варианты."""
    for n in names:
        if n in node.inputs:
            node.inputs[n].default_value = value
            return True
    return False

def principled_metal(name, color, roughness):
    mat = bpy.data.materials.new(name)
    mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF")
    set_input(bsdf, ["Base Color"], color)
    set_input(bsdf, ["Metallic"], 1.0)
    set_input(bsdf, ["Roughness"], roughness)
    return mat


# ───────────────────────── ГЕОМЕТРИЯ ──────────────────────────
def make_body():
    bpy.ops.mesh.primitive_cube_add(size=1.0)
    body = bpy.context.active_object
    body.name = "PhoneBody"
    body.scale = (PW / 2, PD / 2, PH / 2)     # X-ширина, Y-толщина, Z-высота
    bpy.ops.object.transform_apply(scale=True)

    bev = body.modifiers.new("Bevel", "BEVEL")
    bev.width = 0.028
    bev.segments = 8
    bev.limit_method = "ANGLE"
    bpy_shade_smooth(body)
    body.data.materials.append(principled_metal("Body", PURPLE_BODY, 0.35))
    return body

def bpy_shade_smooth(obj):
    for p in obj.data.polygons:
        p.use_smooth = True

def make_back_logo():
    """Плоскость с логотипом на крышке (-Y). Альфа PNG = маска инкрустации."""
    if not os.path.exists(LOGO_PATH):
        print(f"[scene] !!! логотип не найден: {LOGO_PATH}")
        return None

    bpy.ops.mesh.primitive_plane_add(size=1.0)
    plane = bpy.context.active_object
    plane.name = "BackLogo"
    plane.scale = (0.26, 0.26, 1.0)
    plane.rotation_euler = (math.radians(90), 0, 0)   # в плоскость XZ, нормаль -Y
    plane.location = (0, -(PD / 2) - 0.001, 0.18)      # чуть выше центра крышки
    bpy.ops.object.transform_apply(scale=True, rotation=True)

    mat = bpy.data.materials.new("BackLogo")
    mat.use_nodes = True
    nt = mat.node_tree
    nt.nodes.clear()

    tex = nt.nodes.new("ShaderNodeTexImage")
    tex.image = bpy.data.images.load(LOGO_PATH, check_existing=True)
    tex.image.colorspace_settings.name = "Non-Color"   # альфу как маску, не как цвет

    metal = nt.nodes.new("ShaderNodeBsdfPrincipled")
    set_input(metal, ["Base Color"], PURPLE_LOGO)
    set_input(metal, ["Metallic"], 1.0)
    set_input(metal, ["Roughness"], 0.12)              # полированная инкрустация

    transp = nt.nodes.new("ShaderNodeBsdfTransparent")
    mix    = nt.nodes.new("ShaderNodeMixShader")
    out    = nt.nodes.new("ShaderNodeOutputMaterial")

    # Fac=alpha: 0 → прозрачно (видно корпус), 1 → металл-логотип
    nt.links.new(tex.outputs["Alpha"], mix.inputs["Fac"])
    nt.links.new(transp.outputs["BSDF"], mix.inputs[1])
    nt.links.new(metal.outputs["BSDF"], mix.inputs[2])
    nt.links.new(mix.outputs["Shader"], out.inputs["Surface"])

    plane.data.materials.append(mat)
    return plane

def make_screen():
    """Тёмный экран на фронте (+Y). Пока выключен; оживёт в анимации."""
    bpy.ops.mesh.primitive_plane_add(size=1.0)
    scr = bpy.context.active_object
    scr.name = "Screen"
    scr.scale = (PW / 2 - 0.03, PH / 2 - 0.03, 1.0)
    scr.rotation_euler = (math.radians(90), 0, 0)
    scr.location = (0, (PD / 2) + 0.001, 0)
    bpy.ops.object.transform_apply(scale=True, rotation=True)

    mat = bpy.data.materials.new("Screen")
    mat.use_nodes = True
    bsdf = mat.node_tree.nodes.get("Principled BSDF")
    set_input(bsdf, ["Base Color"], (0.01, 0.01, 0.015, 1.0))
    set_input(bsdf, ["Roughness"], 0.08)
    scr.data.materials.append(mat)
    return scr


# ─────────────────────── СВЕТ / КАМЕРА ────────────────────────
def add_area(name, loc, energy, size, rot=(0, 0, 0)):
    light = bpy.data.lights.new(name, "AREA")
    light.energy = energy
    light.size = size
    obj = bpy.data.objects.new(name, light)
    obj.location = loc
    obj.rotation_euler = rot
    bpy.context.collection.objects.link(obj)
    return obj

def setup_lighting():
    # трёхточка: key спереди-сбоку, fill слабее с другой стороны, rim сзади-сверху
    add_area("Key",  (-2.2, -2.6, 2.2), 700, 2.5, (math.radians(55), 0, math.radians(-35)))
    add_area("Fill", ( 2.6, -1.8, 0.6), 200, 3.0, (math.radians(75), 0, math.radians(40)))
    add_area("Rim",  ( 0.8,  2.8, 2.6), 500, 1.5, (math.radians(120), 0, math.radians(20)))

def setup_camera():
    cam_data = bpy.data.cameras.new("Cam")
    cam_data.lens = 85                                  # «продуктовый» телевик
    cam = bpy.data.objects.new("Cam", cam_data)
    bpy.context.collection.objects.link(cam)

    side = -1 if CONFIG["view"] == "back" else 1        # back → камера за крышкой
    cam.location = (1.1, side * 3.4, 0.5)

    target = bpy.data.objects.new("CamTarget", None)
    target.location = (0, 0, 0.05)
    bpy.context.collection.objects.link(target)
    track = cam.constraints.new("TRACK_TO")
    track.target = target
    track.track_axis = "TRACK_NEGATIVE_Z"
    track.up_axis = "UP_Y"

    bpy.context.scene.camera = cam
    return cam

def setup_world():
    world = bpy.data.worlds.new("World")
    bpy.context.scene.world = world
    world.use_nodes = True
    bg = world.node_tree.nodes.get("Background")
    bg.inputs["Color"].default_value = WORLD_BG
    bg.inputs["Strength"].default_value = 0.15


# ──────────────────────────── РЕНДЕР ──────────────────────────
def try_enable_gpu():
    try:
        prefs = bpy.context.preferences.addons["cycles"].preferences
        for backend in ("OPTIX", "CUDA", "HIP", "METAL", "ONEAPI"):
            try:
                prefs.compute_device_type = backend
                prefs.get_devices()
                gpus = [d for d in prefs.devices if d.type != "CPU"]
                if gpus:
                    for d in prefs.devices:
                        d.use = (d.type != "CPU")
                    bpy.context.scene.cycles.device = "GPU"
                    print(f"[scene] GPU включён: {backend} ({len(gpus)} устр.)")
                    return
            except Exception:
                continue
        print("[scene] GPU не найден — рендер на CPU")
    except Exception as e:
        print(f"[scene] GPU init пропущен: {e}")

def setup_render():
    sc = bpy.context.scene
    sc.render.engine = "CYCLES"
    sc.cycles.samples = CONFIG["samples"]
    sc.cycles.use_denoising = True
    sc.render.resolution_x = CONFIG["res_x"]
    sc.render.resolution_y = CONFIG["res_y"]
    sc.render.resolution_percentage = 100
    sc.render.image_settings.file_format = "PNG"
    sc.render.image_settings.color_mode = "RGBA"
    sc.view_settings.view_transform = "AgX"             # приятный филмик-тон
    if CONFIG["use_gpu"]:
        try_enable_gpu()
    out = os.path.join(OUT_DIR, f"{CONFIG['out_name']}_{CONFIG['view']}.png")
    sc.render.filepath = out
    return out


# ──────────────────────────── MAIN ────────────────────────────
def main():
    print(f"[scene] repo root: {ROOT}")
    clear_scene()
    make_body()
    make_back_logo()
    make_screen()
    setup_lighting()
    setup_camera()
    setup_world()
    out = setup_render()

    # При headless-запуске рендерим сразу; в GUI оставляем сцену для осмотра.
    if bpy.app.background:
        bpy.ops.render.render(write_still=True)
        print(f"[scene] рендер сохранён: {out}")
    else:
        print(f"[scene] сцена собрана. F12 для рендера → сохранится в {out}")

main()
