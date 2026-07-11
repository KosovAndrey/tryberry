"""
scene.py — импорт реальной модели айфона из assets/, наш свет/камера/фон, рендер.

Что делает сам:
  1. находит модель (.glb/.gltf/.blend) в render/iphone/assets/;
  2. импортирует её;
  3. печатает инвентарь (объекты/материалы) в System Console;
  4. ставит тёмный фон + солнечный 3-точечный свет + камеру под модель;
  5. рендерит в render/iphone/out/.

От тебя: Pull → Run Script → дождись двух рендеров → пришли из out/:
  • still_front.png  — фронт, ПРОЗРАЧНЫЙ фон, экран выключен (тёмное стекло);
  • still_back.png   — крышка с логотипом;
  • screen_rect.json — прямоугольник экрана в % кадра (для HTML-оверлея на сайте).
(Run сам рендерит оба вида, F12 не нужен.)
ВКЛЮЧИ КОНСОЛЬ: Window → Toggle System Console (там инвентарь и логи).

Экран специально выключен: живой UI бота (бабблы) кладём HTML-оверлеем поверх
рендера на сайте — по координатам из screen_rect.json.

3D-РАЗВОРОТ (интро): поставь CONFIG["turn"]=True и Run — отрендерится
PNG-секвенция разворота 180° (крышка → анфас) в out/turn/. Последний кадр
совпадает со still_front (та же камера). Потом в WSL: bash render/iphone/encode_turn.sh.
"""

import os
import glob
import math
import bpy
from mathutils import Vector, Matrix

# ─────────────────────────── КОНФИГ ───────────────────────────
CONFIG = {
    "res_x":   1080,
    "res_y":   1920,
    "samples": 96,          # превью; для финала поднимем
    "use_gpu": True,
    # Сжатие корпуса по ширине (1.0 = выкл). Оказалось НЕ нужно: у модели
    # ДИСПЛЕЙ (BsXHDwLKqtDOfrW) уже имеет аспект 0.4617 ≈ видео 1320:2868
    # (0.4602). Кривизна на сайте была от того, что CSS-коробка строилась по
    # переднему стеклу С РАМКОЙ (0.47-0.48), а не по дисплею. Видео сажаем
    # в проекцию дисплея из screen_rect.json; механизм slim оставлен на всякий.
    "slim_x": 1.0,
    "rainbow": False,       # DEBUG: каждый материал в свой цвет (для опознания деталей)
    "screen_off": True,     # экран = выключенное тёмное стекло (UI кладём HTML-оверлеем на сайте)
    "transparent": True,    # прозрачный фон (film) — корпус «парит» поверх hero-фона сайта

    # ── 3D-разворот-интро (анимация: с крышки 180° → анфас) ──
    "turn": False,          # True = рендерим PNG-секвенцию разворота вместо стиллов
    "turn_seconds": 2.0,    # длительность разворота
    "turn_fps": 60,         # 60 — плавность на быстрой фазе вращения (30 дёргалось)
    "turn_samples": 48,     # на кадр анимации хватает меньше (есть denoise)
    "turn_reverse": False,  # True = крутить в другую сторону
    "turn_shutter": 0.35,   # моушен-блюр (0 = выкл): смаз на быстрой фазе, киношно

    # ── Студийные стрипы (пара вертикальных софтбоксов, place_strips) ──
    # ЕДИНСТВЕННЫЙ источник бликов и на корпусе, и на стекле — как в реальной
    # продуктовой съёмке телефонов: два стрипа слева/справа от камеры.
    # Азимут ±20° выбран так, что В ФИНАЛЬНОМ АНФАСЕ отражения лежат ЗА краями
    # экрана (экран чистый, бликов нет), а при развороте правый стрип физически
    # проезжает по стеклу и уходит за край на ease-out (блик только в движении);
    # левый так же обтекает крышку в первой половине разворота.
    "strips": {
        "azimuth_deg": 20.0,  # угол пары от оси камеры; меньше = вайп позже/ближе к финалу,
                              # НО < ~16° полоса останется на экране в статике
        "dist":   1.6,        # расстояние стрипов от центра модели (в size)
        "width":  0.5,        # ширина стрипа (в size) = ширина полосы блика
        "length": 3.0,        # высота стрипа (в size) — полоса на всю высоту корпуса
        "tilt_deg": 8.0,      # наклон длинной оси от вертикали (лёгкая диагональ блика)
        "energy": 1200.0,     # ⚙ яркость пары (крутить 500..2500)
    },
    # Пробные кадры БЕЗ рендера секвенции: телефон за N° до финала разворота —
    # видно, как блик едет по стеклу (5..20) и обтекает крышку (90/170).
    # Рендерятся при обычном Run scene.py → out/still_probe_NNN.png. [] = выкл.
    "probe_deg": [5, 10, 20, 90, 170],
}
# Запуск через turn.py ставит одноразовый env-флаг — редактировать CONFIG не нужно
# (pop: флаг не «залипает» на следующие Run scene.py в той же сессии Blender).
if os.environ.pop("TRYBERRY_TURN", None):
    CONFIG["turn"] = True

# Палитра различимых цветов для debug-радуги (имя, RGB 0-255):
PALETTE = [
    ("RED",        (230,  25,  75)), ("GREEN",      ( 60, 180,  75)),
    ("YELLOW",     (255, 225,  25)), ("BLUE",       (  0, 130, 200)),
    ("ORANGE",     (245, 130,  48)), ("PURPLE",     (145,  30, 180)),
    ("CYAN",       ( 70, 240, 240)), ("MAGENTA",    (240,  50, 230)),
    ("LIME",       (210, 245,  60)), ("PINK",       (250, 150, 200)),
    ("TEAL",       (  0, 160, 160)), ("LAVENDER",   (200, 180, 255)),
    ("BROWN",      (170, 110,  40)), ("MINT",       (150, 255, 195)),
    ("OLIVE",      (160, 160,  20)), ("APRICOT",    (255, 180, 120)),
    ("NAVY",       ( 50,  80, 200)), ("GREY",       (160, 160, 160)),
    ("WHITE",      (255, 255, 255)), ("CRIMSON",    (200,   0,  60)),
    ("SPRING",     (  0, 230, 120)), ("SKYBLUE",    (120, 200, 255)),
    ("HOTPINK",    (255,  90, 160)), ("CHARTREUSE", (140, 230,  10)),
    ("GOLD",       (240, 190,  20)), ("TURQUOISE",  ( 40, 220, 200)),
    ("SALMON",     (250, 130, 110)), ("INDIGO",     ( 90,  40, 200)),
    ("EMERALD",    ( 20, 200, 100)), ("CORAL",      (255, 120,  80)),
    ("VIOLET",     (190,  90, 255)), ("AMBER",      (255, 200,  60)),
]
WORLD_BG = (0.02, 0.012, 0.02, 1.0)   # тёмный berry-фон под цвет сайта

# Наш плам для корпуса (sRGB 0..1). Подбираем по скриншоту.
PLUM_SRGB = (0.37, 0.086, 0.25)       # ~#5e1640 — глубокий berry/plum

# Роли материалов опознаны по rainbow_map (цвет → деталь).
# Чёрные: линзы/сенсоры камер, вспышка, dynamic island, внутрянка:
BLACK_MATS = {
    "AYSuIKiLIvlGvvQ",   # центры объективов (RED)
    "CVcxUAKakDuRdCf",   # сенсор (YELLOW)
    "EOPlztmjOhyFwUF",   # сенсор (BLUE)
    "EiHyBykxPjKZBgf",   # стекло линзы (ORANGE)
    "NUlImpGytyodpBy",   # вспышка/датчики (LIME)
    "hqDUrVMlYhzYusu",   # линза (SPRING)
    "QEOvfSZiwySWiUk",   # линза (TEAL)
    "jKYrqbVsPDbEaqj",   # линза (CHARTREUSE)
    "nwfiSfJrPZRLBAj",   # детали камеры (GOLD)
    "uFgsppDNoPNkBqW",   # внутрянка (EMERALD)
    "vUNWrAqjHCArnzh",   # dynamic island (CORAL)
    "YVjGRIfwSbFphGH",   # сенсор (NAVY)
    "ybSvSfarxzoBKlb",   # мелкий сенсор (AMBER)
    "UiBplfShRNPzcmF",   # стекло линзы (MINT)
    "awYxKfiOpRgQIxD",   # передний сенсор/FaceID (GREY)
    "ieDmCkHnOnSIOcm",   # пилюля dynamic island (HOTPINK, меш 21x6 спереди)
}
# Не трогаем: экран + переднее стекло (заменим UI отдельно):
KEEP_MATS = {"BsXHDwLKqtDOfrW", "LqxrKBoiOXSOFqs"}
# Выключенный экран: ГЛУБОКИЙ чёрный, не серый. Серая пелена была отражением
# софтбоксов во всём стекле (roughness 0.06 + coat 1.0 размазывали блик по
# экрану). Гасим силу отражения (Specular IOR Level) и лак — у камер/острова
# (BLACK_MATS) глянец остаётся полный, поэтому они читаются ПОВЕРХ чёрного экрана.
SCREEN_BLACK = (0.0, 0.0, 0.0)   # база экрана
SCREEN_ROUGH = 0.05              # стекло остаётся гладким
SCREEN_SPEC  = 0.2               # 1.0 = серая пелена; 0.2 = лёгкий живой отблеск
SCREEN_COAT  = 0.15              # лак почти убран (у камер остаётся 1.0)
SCREEN_DISPLAY_MAT = "BsXHDwLKqtDOfrW"   # именно дисплей (активная область) — для screen_rect
# Задняя панель вокруг яблока = «старый основной» цвет PLUM_SRGB (светлее корпуса,
# как на реальном айфоне). Корпус/рамку делаем чуть ТЕМНЕЕ через BODY_MUL.
BACK_PANEL_MATS = {"SMUhrjUPCjJkPUK"}
BODY_MUL = 0.80                          # корпус темнее панели (0.7 темнее, 0.9 ближе к панели)
# Все остальные материалы → плам.

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
    plum = srgb_to_linear([c * BODY_MUL for c in PLUM_SRGB]) + [1.0]   # корпус — чуть темнее панели
    back_plum = srgb_to_linear(PLUM_SRGB) + [1.0]                      # задняя панель — старый основной (светлее)

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
        is_glass = mat.name in KEEP_MATS         # экран/переднее стекло
        if is_glass:
            # выключенный экран: глубокий чёрный (см. SCREEN_* над BLACK_MATS).
            # Реальный UI кладём HTML-оверлеем на сайте (screen_rect.json).
            target = list(SCREEN_BLACK) + [1.0] if CONFIG.get("screen_off") else None
        elif is_black:
            target = [0.0, 0.0, 0.0, 1.0]        # линзы/сенсоры/dynamic island
        elif mat.name in BACK_PANEL_MATS:
            target = back_plum                   # задняя панель вокруг яблока — темнее
        else:
            target = plum                        # всё остальное — корпус
        do = target is not None

        if do:
            unlink_base_color(mat, bc)
            bc.default_value = target
            if "Metallic" in p.inputs:
                # плам — анодированный металл 0.85 (металл=1 выглядел серым зеркалом)
                p.inputs["Metallic"].default_value = 0.0 if (is_black or is_glass) else 0.85
            if "Roughness" in p.inputs:
                if is_glass:
                    p.inputs["Roughness"].default_value = SCREEN_ROUGH
                elif is_black:
                    # снова глянцевое чёрное стекло: раньше серело из-за ярких обоев
                    # ВКЛючённого экрана — теперь экран выключен, глянец безопасен
                    p.inputs["Roughness"].default_value = 0.10
                else:
                    r = p.inputs["Roughness"].default_value or 0.3
                    p.inputs["Roughness"].default_value = min(0.45, max(0.15, r))
            # «мокрый» лак: камерам/острову полный (читаются поверх чёрного
            # экрана), экрану почти нет + гасим силу отражения — иначе серая пелена
            if is_glass or is_black:
                coat = p.inputs.get("Coat Weight")
                if coat is not None:
                    coat.default_value = SCREEN_COAT if is_glass else 1.0
                    cr = p.inputs.get("Coat Roughness")
                    if cr is not None:
                        cr.default_value = 0.03
            if is_glass:
                spec = p.inputs.get("Specular IOR Level")
                if spec is not None:
                    spec.default_value = SCREEN_SPEC
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


def rainbow_materials():
    """DEBUG: каждый материал → свой светящийся цвет из PALETTE. Легенда (цвет →
    материал → меши) в out/rainbow_map.txt. Порядок — по имени материала (стабильно)."""
    usage = {}
    for o in bpy.data.objects:
        if o.type != "MESH":
            continue
        for s in o.material_slots:
            if s.material:
                usage.setdefault(s.material.name, []).append(
                    (o.name, tuple(round(d, 3) for d in o.dimensions)))

    mats = sorted(bpy.data.materials, key=lambda m: m.name)
    lines = ["--- RAINBOW MAP (ЦВЕТ -> материал -> меши) ---"]
    for i, mat in enumerate(mats):
        p = get_principled(mat)
        if not p:
            continue
        cname, rgb = PALETTE[i % len(PALETTE)]
        col = srgb_to_linear([c / 255 for c in rgb]) + [1.0]
        bc = p.inputs.get("Base Color")
        if bc:
            unlink_base_color(mat, bc)
            bc.default_value = (0.0, 0.0, 0.0, 1.0)
        em = p.inputs.get("Emission Color") or p.inputs.get("Emission")
        if em:
            for link in list(mat.node_tree.links):
                if link.to_socket == em:
                    mat.node_tree.links.remove(link)
            em.default_value = col
        if "Emission Strength" in p.inputs:
            p.inputs["Emission Strength"].default_value = 1.0
        lines.append(f"{cname:<11} -> {mat.name}")
        for mname, dims in usage.get(mat.name, [])[:6]:
            lines.append(f"      {mname} {dims}")

    report = "\n".join(lines)
    print(report)
    try:
        path = os.path.join(OUT_DIR, "rainbow_map.txt")
        with open(path, "w", encoding="utf-8") as f:
            f.write(report)
        print(f"[rainbow] легенда: {path}")
    except Exception as e:
        print(f"[rainbow] не записал rainbow_map.txt: {e}")


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
    # up_axis — ЛОКАЛЬНАЯ ось камеры, и она НЕ должна совпадать с track (лок. Z).
    # UP_Y = локальная Y вверх (к мировому +Z) — стандарт; UP_Z конфликтовал с track -Z.
    c.up_axis = "UP_Y"
    bpy.context.scene.camera = cam
    return cam, tgt


def place_camera(cam, tgt, center, size, view):
    # front — СТРОГО анфас (экран = чистый прямоугольник под DOM-оверлей);
    # back — ¾ ракурс, видно крышку/логотип (для beauty/Reels)
    d = Vector((0.0, -1.0, 0.0)) if view == "front" else Vector((-0.9, 1.0, 0.35))
    cam.location = center + d.normalized() * size * 3.2
    tgt.location = center


def add_sun(name, rot, energy):
    la = bpy.data.lights.new(name, "SUN")     # солнце — не зависит от масштаба модели
    la.energy = energy
    obj = bpy.data.objects.new(name, la)
    obj.rotation_euler = rot
    bpy.context.collection.objects.link(obj)


# Пара студийных стрипов (см. CONFIG["strips"]): вертикальные area-панели
# слева/справа от камеры — единый источник бликов на корпусе И стекле.
# Стоят в мире неподвижно: при развороте отражения «текут» по телефону,
# в финальном анфасе лежат за краями экрана (экран чистый).
def place_strips(center, size):
    s = CONFIG.get("strips") or {}
    if not s:
        return
    az_deg = float(s["azimuth_deg"])
    tilt = math.radians(float(s["tilt_deg"]))
    for name, sgn in (("StripR", 1.0), ("StripL", -1.0)):
        a = math.radians(az_deg) * sgn
        d = Vector((math.sin(a), -math.cos(a), 0.0))       # от центра к стрипу (перед = -Y)
        pos = center + d * float(s["dist"]) * size
        la = bpy.data.lights.new(name, "AREA")
        la.shape = "RECTANGLE"
        la.size = float(s["width"]) * size
        la.size_y = float(s["length"]) * size
        la.energy = float(s["energy"]) * max(size, 1e-4) ** 2
        obj = bpy.data.objects.new(name, la)
        bpy.context.collection.objects.link(obj)
        # ориентация: панель смотрит в центр (-Z_local = -d), длинная ось —
        # вертикаль с лёгким наклоном tilt (зеркально для L/R)
        p = Vector((math.cos(a), math.sin(a), 0.0))        # горизонталь ⊥ направлению
        ay = (Vector((0.0, 0.0, 1.0)) * math.cos(tilt) + p * math.sin(tilt) * sgn).normalized()
        az = d.normalized()
        ax = ay.cross(az)
        obj.matrix_world = Matrix((
            (ax.x, ay.x, az.x, pos.x),
            (ax.y, ay.y, az.y, pos.y),
            (ax.z, ay.z, az.z, pos.z),
            (0.0, 0.0, 0.0, 1.0)))
    print(f"[strips] пара стрипов: азимут ±{az_deg}°, dist={s['dist']}×size, "
          f"{s['width']}×{s['length']}×size, tilt={s['tilt_deg']}°, E={s['energy']}")


def render_probes(cam, tgt, center, size, pivot):
    """Стиллы «за N° до финала разворота» (CONFIG["probe_deg"]): видно, как блик
    едет по стеклу/крышке, БЕЗ рендера секвенции. Камера — анфас, самплы как у
    анимации (быстро). → out/still_probe_NNN.png"""
    degs = list(CONFIG.get("probe_deg") or [])
    if not degs:
        return
    place_camera(cam, tgt, center, size, "front")
    sc = bpy.context.scene
    keep = sc.cycles.samples
    sc.cycles.samples = CONFIG["turn_samples"]
    sign = -1.0 if CONFIG.get("turn_reverse") else 1.0
    for deg in degs:
        pivot.rotation_euler = (0.0, 0.0, sign * math.radians(float(deg)))
        bpy.context.view_layer.update()
        out = os.path.join(OUT_DIR, f"still_probe_{int(deg):03d}.png")
        sc.render.filepath = out
        print(f"[probe] {deg}° до финала…")
        bpy.ops.render.render(write_still=True)
        print(f"[probe]   готово: {out}")
    pivot.rotation_euler = (0.0, 0.0, 0.0)
    bpy.context.view_layer.update()
    sc.cycles.samples = keep


def setup_lights(center, size, target):
    add_sun("Key",  (math.radians(55), math.radians(10), math.radians(-40)), 4.0)
    add_sun("Fill", (math.radians(70), 0,                math.radians(60)),  1.5)
    add_sun("Rim",  (math.radians(120), 0,               math.radians(150)), 3.0)
    # пара студийных стрипов — все блики (корпус + стекло) из одного источника
    place_strips(center, size)


def setup_world():
    w = bpy.data.worlds.new("World")
    bpy.context.scene.world = w
    w.use_nodes = True
    bg = w.node_tree.nodes.get("Background")
    bg.inputs["Color"].default_value = WORLD_BG
    # фон невидим (film_transparent), но мир заливает мягкое отражение в глянце —
    # чуть ярче, чтобы чёрное стекло читалось как стекло, а не как «дыра»
    bg.inputs["Strength"].default_value = 0.6


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
    sc.render.image_settings.color_mode = "RGBA"       # альфа для прозрачного фона
    # прозрачный фон (film) — чтобы корпус лёг PNG-оверлеем поверх hero-фона сайта
    sc.render.film_transparent = bool(CONFIG.get("transparent") and not CONFIG.get("rainbow"))
    # в радуге — Standard, чтобы цвета были чистые и различимые; иначе AgX
    sc.view_settings.view_transform = "Standard" if CONFIG.get("rainbow") else "AgX"
    if CONFIG["use_gpu"]:
        try_enable_gpu()


# ──────────────── ПРОЕКЦИЯ ЭКРАНА ДЛЯ HTML-ОВЕРЛЕЯ ─────────────
def _proj_verts_of_mats(scene, cam, w2c, matset):
    """Проецирует вершины ГРАНЕЙ, у которых материал ∈ matset (по material_index),
    а не габарит объекта — точный контур экрана даже если материал на большом меше.
    Возвращает (xs, ys в координатах кадра; wmin, wmax — мировой bbox этих граней)."""
    xs, ys = [], []
    wmin = [1e18, 1e18, 1e18]
    wmax = [-1e18, -1e18, -1e18]
    for o in bpy.data.objects:
        if o.type != "MESH":
            continue
        slots = [i for i, s in enumerate(o.material_slots)
                 if s.material and s.material.name in matset]
        if not slots:
            continue
        slots = set(slots)
        me, mw = o.data, o.matrix_world
        for poly in me.polygons:
            if poly.material_index in slots:
                for vi in poly.vertices:
                    wv = mw @ me.vertices[vi].co
                    co = w2c(scene, cam, wv)
                    xs.append(co.x); ys.append(co.y)
                    for k in range(3):
                        wmin[k] = min(wmin[k], wv[k]); wmax[k] = max(wmax[k], wv[k])
    return xs, ys, wmin, wmax


def export_screen_rect(cam, view):
    """Проецирует грани дисплея через камеру и пишет прямоугольник экрана в % кадра
    → out/screen_rect.json. По этим числам сайт ставит живой HTML-экран (бабблы бота)
    точно в «дырку» рендера. Только для front (анфас)."""
    if view != "front":
        return
    try:
        from bpy_extras.object_utils import world_to_camera_view as w2c
    except Exception as e:
        print(f"[screen] нет world_to_camera_view ({e}) — screen_rect не записан")
        return
    scene = bpy.context.scene
    # точный контур дисплея по граням; фолбэк — всё переднее стекло (KEEP_MATS)
    xs, ys, wmin, wmax = _proj_verts_of_mats(scene, cam, w2c, {SCREEN_DISPLAY_MAT})
    src = f"display:{SCREEN_DISPLAY_MAT}"
    if not xs:
        xs, ys, wmin, wmax = _proj_verts_of_mats(scene, cam, w2c, KEEP_MATS)
        src = "KEEP_MATS(fallback)"
    if not xs:
        print("[screen] грани экрана не найдены — screen_rect не записан")
        return
    rminx, rmaxx = min(xs), max(xs)              # сырые (могут выходить за [0,1])
    rminy, rmaxy = min(ys), max(ys)
    x0, x1 = max(0.0, rminx), min(1.0, rmaxx)    # клампим в кадр для CSS
    y0, y1 = max(0.0, rminy), min(1.0, rmaxy)
    dsize = [round(wmax[k] - wmin[k], 4) for k in range(3)]   # мировые габариты экрана (dx,dy,dz)
    # ДИАГНОСТИКА: по каждому материалу-кандидату — проекция и мировые габариты,
    # чтобы понять, какой из них настоящий видимый экран (тонкая ось = нормаль).
    cand = {}
    for m in sorted(KEEP_MATS | {SCREEN_DISPLAY_MAT}):
        cxs, cys, cwmn, cwmx = _proj_verts_of_mats(scene, cam, w2c, {m})
        if not cxs:
            continue
        cand[m] = {
            "proj_x": [round(min(cxs), 3), round(max(cxs), 3)],
            "proj_y": [round(min(cys), 3), round(max(cys), 3)],
            "world_size": [round(cwmx[k] - cwmn[k], 4) for k in range(3)],
            "nverts": len(cxs),
        }
    rect = {                                     # проценты от размера still_front.png
        "left":   round(x0 * 100, 3),
        "top":    round((1.0 - y1) * 100, 3),    # camera-view y=0 внизу → CSS top сверху
        "width":  round((x1 - x0) * 100, 3),
        "height": round((y1 - y0) * 100, 3),
        "res_x":  scene.render.resolution_x,
        "res_y":  scene.render.resolution_y,
        "source": src,
        "raw":    {"minx": round(rminx, 4), "maxx": round(rmaxx, 4),
                   "miny": round(rminy, 4), "maxy": round(rmaxy, 4)},
        "display_world_size": dsize,             # диагностика: тонкая ось = нормаль экрана
        "candidates": cand,                      # по каждому KEEP-материалу: проекция + габариты
        "note":   "% от кадра still_front.png; CSS-оверлей экрана позиционируется по ним",
    }
    print(f"[screen] display_world_size(dx,dy,dz)={dsize} — тонкая ось = нормаль экрана")
    import json
    try:
        path = os.path.join(OUT_DIR, "screen_rect.json")
        with open(path, "w", encoding="utf-8") as f:
            json.dump(rect, f, ensure_ascii=False, indent=2)
        print(f"[screen] screen_rect → {path}: {rect}")
    except Exception as e:
        print(f"[screen] не записал screen_rect.json: {e}")


# ─────────────────── СЖАТИЕ КОРПУСА ПОД АСПЕКТ ВИДЕО ───────────
def apply_slim(meshes):
    """Сужает модель по мировой X (ширина телефона в анфас) на CONFIG["slim_x"].
    Через пустышку-родителя в центре модели: при развороте (TurnPivot выше по
    иерархии) сжатие крутится вместе с телефоном — как физически узкий корпус.
    Вызывать ПОСЛЕ импорта, ДО setup_turn_pivot/камеры/света."""
    f = float(CONFIG.get("slim_x") or 1.0)
    if abs(f - 1.0) < 1e-6:
        return
    center, _ = world_bounds(meshes)
    piv = bpy.data.objects.new("SlimPivot", None)
    piv.location = center
    bpy.context.collection.objects.link(piv)
    for o in list(bpy.context.scene.objects):
        if o is piv or o.parent is not None:
            continue
        o.parent = piv
        o.matrix_parent_inverse = piv.matrix_world.inverted()
    piv.scale = (f, 1.0, 1.0)
    bpy.context.view_layer.update()              # обновить матрицы до замера bounds
    print(f"[slim] корпус сужен по X: {f} (стекло → аспект видео 1320:2868)")


# ─────────────────────── 3D-РАЗВОРОТ (turn) ───────────────────
def setup_turn_pivot(center):
    """Пустышка-пивот в центре модели; ВСЕ корневые объекты модели — под неё.
    Вызывать ДО создания камеры/света (в сцене только импортированная модель).
    Крутим сам телефон, а не камеру: свет/софтбоксы остаются в мире → блики
    «текут» по корпусу при повороте (кинематографично), а финальный кадр
    гарантированно совпадает со still_front (та же камера-анфас)."""
    pivot = bpy.data.objects.new("TurnPivot", None)
    pivot.location = center
    bpy.context.collection.objects.link(pivot)
    for o in list(bpy.context.scene.objects):
        if o is pivot or o.parent is not None:
            continue
        o.parent = pivot
        o.matrix_parent_inverse = pivot.matrix_world.inverted()
    return pivot


def animate_turn(pivot):
    """Ключи: кадр 1 = крышка к нам (180°), последний = 0° (анфас, якорный кадр).
    Bezier auto-clamped даёт плавный ease-in-out — медленный старт и мягкую
    остановку. Возвращает (frame_start, frame_end)."""
    sc = bpy.context.scene
    n = max(2, round(CONFIG["turn_seconds"] * CONFIG["turn_fps"]))
    sc.frame_start, sc.frame_end = 1, n
    sc.render.fps = CONFIG["turn_fps"]

    sign = -1.0 if CONFIG.get("turn_reverse") else 1.0
    pivot.rotation_mode = "XYZ"
    pivot.rotation_euler = (0.0, 0.0, sign * math.pi)
    pivot.keyframe_insert("rotation_euler", index=2, frame=1)
    pivot.rotation_euler = (0.0, 0.0, 0.0)
    pivot.keyframe_insert("rotation_euler", index=2, frame=n)
    # CUBIC ease-in-out: мягкий разгон и мягкая остановка выраженнее, чем у
    # дефолтного Bezier auto-clamped (тот почти линеен в середине).
    # Blender 5.x: у Action больше нет .fcurves (layered actions) — идём через
    # layers→strips→channelbags; если API снова сменится — не падаем (дефолт ок).
    try:
        act = pivot.animation_data.action
        fcs = getattr(act, "fcurves", None)
        if fcs is None:
            fcs = [fc for layer in act.layers for strip in layer.strips
                   for cb in strip.channelbags for fc in cb.fcurves]
        for fc in fcs:
            for kp in fc.keyframe_points:
                kp.interpolation = "CUBIC"
                kp.easing = "EASE_IN_OUT"
    except Exception as e:
        print(f"[turn] не выставил интерполяцию ключей ({e}) — дефолт тоже плавный, едем дальше")
    return 1, n


def render_turn(cam, tgt, center, size, pivot):
    """PNG-секвенция разворота (RGBA, прозрачный фон) → out/turn/turn_####.png.
    Камера — СТРОГО анфас (как still_front): последний кадр == still_front,
    на сайте видео подменяется на PNG без скачка. Дальше энкод: encode_turn.sh."""
    place_camera(cam, tgt, center, size, "front")
    bpy.context.view_layer.update()
    f0, f1 = animate_turn(pivot)
    sc = bpy.context.scene
    sc.cycles.samples = CONFIG["turn_samples"]
    if CONFIG.get("turn_shutter"):
        sc.render.use_motion_blur = True
        sc.render.motion_blur_shutter = CONFIG["turn_shutter"]
    turn_dir = os.path.join(OUT_DIR, "turn")
    os.makedirs(turn_dir, exist_ok=True)
    for old in glob.glob(os.path.join(turn_dir, "turn_*.png")):
        os.remove(old)                           # стейл-кадры прошлых длительностей — вон
    sc.render.filepath = os.path.join(turn_dir, "turn_")
    print(f"[turn] рендерю {f1} кадров ({CONFIG['turn_seconds']}s @ {CONFIG['turn_fps']}fps, "
          f"{CONFIG['turn_samples']} samples) → {turn_dir}")
    bpy.ops.render.render(animation=True)
    print(f"[turn] готово: {turn_dir}/turn_0001.png .. turn_{f1:04d}.png")

    # СТИЛЛ ИЗ ТОЙ ЖЕ СЕССИИ: hero-iphone.png/gloss/screen_rect обязаны совпадать
    # с последним кадром видео 1:1. Раньше стилл рендерился отдельным Run — камера
    # чуть отличалась, на сайте был скачок размера (компенсирован CSS-фаджем,
    # после этого рендера фадж убираем). Кадр f1 = поворот 0° (анфас).
    sc.frame_set(f1)
    sc.render.use_motion_blur = False
    sc.cycles.samples = CONFIG["samples"]
    bpy.context.view_layer.update()
    export_screen_rect(cam, "front")
    sc.render.filepath = os.path.join(OUT_DIR, "still_front.png")
    print(f"[turn] рендерю still_front (кадр {f1}, {CONFIG['samples']} samples) — якорь стыка…")
    bpy.ops.render.render(write_still=True)
    print(f"[turn] готово: {os.path.join(OUT_DIR, 'still_front.png')} + screen_rect.json")
    print("[turn] дальше в WSL: bash render/iphone/encode_turn.sh "
          "(альфа-WebM + все веб-ассеты из этой же сессии)")


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
    if CONFIG.get("rainbow"):
        rainbow_materials()
    else:
        recolor_to_plum()

    apply_slim(meshes)                           # сузить корпус под аспект видео
    center, size = world_bounds(meshes)
    print(f"[scene] центр={tuple(round(c,3) for c in center)} размер={round(size,3)}")
    # пивот нужен и для разворота, и для пробных кадров блика — до камеры/света!
    need_pivot = CONFIG.get("turn") or CONFIG.get("probe_deg")
    pivot = setup_turn_pivot(center) if need_pivot else None
    setup_world()
    setup_render()
    cam, tgt = create_camera()
    setup_lights(center, size, tgt)              # стрипы позиционируются вокруг center

    # СЕКВЕНЦИЯ — ТОЛЬКО по флагу turn (его ставит turn.py); наличие пивота
    # само по себе — НЕ повод рендерить 120 кадров (был такой баг: пробы
    # создавали пивот → обычный Run уходил в полный рендер разворота)
    if CONFIG.get("turn"):
        render_turn(cam, tgt, center, size, pivot)
        return

    # рендерим сразу оба вида — front и back — за один Run
    sc = bpy.context.scene
    for view in ("front", "back"):
        place_camera(cam, tgt, center, size, view)
        bpy.context.view_layer.update()          # применить TRACK_TO ДО проекции (иначе матрица камеры устаревшая)
        export_screen_rect(cam, view)            # прямоугольник экрана для HTML-оверлея
        out = os.path.join(OUT_DIR, f"still_{view}.png")
        sc.render.filepath = out
        print(f"[scene] рендерю {view}…")
        bpy.ops.render.render(write_still=True)
        print(f"[scene]   готово: {out}")
    print("[scene] оба вида готовы: out/still_front.png и out/still_back.png")
    if pivot is not None:
        render_probes(cam, tgt, center, size, pivot)

main()
