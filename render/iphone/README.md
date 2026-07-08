# render/iphone — кинематографичный рендер айфона для hero сайта + Reels

Цель: тёмно-фиолетовый айфон под цвет сайта разворачивается с крышки на 180°,
экран загорается (часы + наш логотип), разблокировка, прилёт пуша от бота.
Полный 3D-рендер в Blender → веб-видео для `web/` и контент для Instagram.

## Поток работы (Claude ↔ ты)
1. Claude пишет/правит скрипты здесь и пушит в ветку `feat/hero-iphone-render`.
2. Ты на **Windows** делаешь `git pull` и запускаешь в Blender.
3. Кидаешь Claude скриншот/рендер из `render/iphone/out/` — правим по картинке.

Рендеры (`out/`) и тяжёлые модели в гит НЕ коммитятся — только финальное видео в `web/`.

## Установка на Windows (один раз)
- Blender 5.1.2 — уже стоит.
- Репозиторий клонировать на винду (рядом с WSL-копией, не вместо):
  ```
  git clone git@gitlab.com:KosovAndrey/tryberrybot.git
  ```
  (нужен SSH-ключ на винде; либо HTTPS-клон). Затем:
  ```
  git checkout feat/hero-iphone-render
  ```
- FFmpeg — для энкода (можно в WSL, там удобнее).

## Запуск рендера
Из папки репозитория на винде.

**Вариант A — GUI (смотрим вьюпорт, правим свет):**
1. Blender → вкладка **Scripting**.
2. **Open** → `render/iphone/scene.py` → **Run Script** (▶).
3. Сцена соберётся; **F12** — рендер. Сохранится в `render/iphone/out/`.

**Вариант B — headless (быстрый финальный рендер):**
```
blender --background --python render/iphone/scene.py
```
(если `blender` не в PATH — полный путь к `blender.exe`).

## Настройки — в начале scene.py, словарь `CONFIG`
- `view`: `"back"` (крышка с логотипом) или `"front"` (экран).
- `res_x/res_y`: по умолчанию 1080×1920 (9:16, под hero и Reels).
- `samples`: 128 (превью). Для финала — 512.
- `use_gpu`: пытается OptiX/CUDA, иначе CPU.

## Где мы сейчас
Стиллы ГОТОВЫ и на сайте: `web/hero-iphone.png`/`.webp` (прозрачный фон, экран
выключен) + живой HTML-экран оверлеем по `out/screen_rect.json` — композит в
`web/index.html` (lockscreen → Face ID unlock → бабблы пуша, всё на DOM).

**Текущая стадия — 3D-разворот-интро** (последняя):
1. На Windows: `git pull`, в `scene.py` поставить `CONFIG["turn"] = True`,
   Run Script → PNG-секвенция разворота (крышка 180° → анфас) в `out/turn/`
   (~84 кадра, 2.8s @ 30fps; телефон крутится, камера/свет стоят —
   последний кадр 1-в-1 `still_front`).
2. В WSL: `bash render/iphone/encode_turn.sh /mnt/c/<путь к out/turn>` —
   альфа-WebM → `web/hero-iphone-turn.webm`.
3. Подключение на сайте (после п.2): `<video>` поверх `.phone` → по `ended`
   подмена на PNG + включение DOM-экрана. Safari (нет VP9-альфы) — фолбэк
   сразу на PNG без интро.

Ручки: `turn_seconds` / `turn_fps` / `turn_samples` / `turn_reverse` в CONFIG;
яркость бликов — `SOFTBOX_ENERGY`.
