"""
turn.py — лаунчер РАЗВОРОТА. Открой этот файл в Blender (Scripting → Open) и Run.

Сам исполняет СВЕЖИЙ scene.py с диска (git pull подхватывается автоматически,
Text → Reload не нужен) и включает CONFIG["turn"] через одноразовый env-флаг.

Рендер секвенции идёт ПРЯМО ВО ВРЕМЯ Run (F12 не нужен!) — ~84 кадра,
прогресс смотри в System Console. Результат: out/turn/turn_0001..0084.png.
Стиллы — как раньше, отдельным Run самого scene.py.
"""

import os
import runpy
import bpy


def _dir():
    for t in bpy.data.texts:
        if t.filepath and os.path.basename(t.filepath).lower() in ("turn.py", "scene.py"):
            return os.path.dirname(bpy.path.abspath(t.filepath))
    try:
        return os.path.dirname(os.path.realpath(__file__))
    except NameError:
        return None


d = _dir()
if not d:
    print("[turn] не нашёл папку render/iphone — открой turn.py через Scripting → Open")
else:
    os.environ["TRYBERRY_TURN"] = "1"            # scene.py сделает pop() — одноразово
    print(f"[turn] запускаю {os.path.join(d, 'scene.py')} в режиме разворота…")
    runpy.run_path(os.path.join(d, "scene.py"))
