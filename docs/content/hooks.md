# Хуки для промо-роликов — рабочий каталог

Дата: 2026-07-24. Канон стратегии: [PROMO-SHORTS-PLAN.md](../PROMO-SHORTS-PLAN.md).
Структура ролика: **[AI-хук 2–3с] → [середина: бот, Remotion] → [endcard]**.
Хук = 3 слоя, которые НЕ дублируют друг друга (система из marketing-skills:ad-creative):

- **Визуал** — что в кадре (Kling: живая ситуация, не предмет) → стопит палец
- **VO** — первая произнесённая фраза → открывает петлю
- **Титр** — крупный текст на монтаже → работает для 85% зрителей без звука

Endcard во всех роликах один: логотип · @tryberrybot · «Проверять цены / вручную /
больше не нужно» · Telegram·VK·MAX · «Бесплатно. Без карты» · tryberry.ru.

Правило честности: заголовок-кликбейт снаружи допустим, только если середина
выполняет обещание. Цифры — из реальных данных, не выдумывать.

**ГЛАВНОЕ ПРАВИЛО (усвоено 2026-07-24 на ошибке):** AI генерит то, чего мы снять
НЕ можем — живого человека в узнаваемой ситуации, среду, эмоцию. Красивый предмет
на градиенте палец НЕ останавливает, это заставка карточки товара. Продукт у нас
уже есть в рендерах: айфон — `web/hero-iphone-turn.webm` (1080×1920, фирменный
цвет, Blender), чат и график — Remotion. Beauty-кадр предмета идёт ВНУТРИ ролика
как вставка, а не как хук.

Промпты пишем под **Kling** (у него отдельное поле негатива — все запреты туда,
основной промпт остаётся чисто описательным). Вертикаль 9:16, 5 сек (в монтаж
берём ~3), люди не говорят (липсинк палится) — драма визуальная, речь в VO.

**Формула промпта** (из `marketing-skills/video/ai-video-prompting.md`):
`[Субъект] + [Действие] + [Движение камеры] + [Стиль] + [Свет] + [Технические]`,
50–100 слов. Движение камеры указывать ВСЕГДА (иначе модель дёргает случайно):
static · slow push · dolly in/out · pan · tilt · orbit · tracking · handheld.
Kling любит простые сцены с малым числом субъектов — не усложнять.

**Три правила, выведенные production-опытом** (`ad-creative/motion-video-ads.md`):
1. ⛔ **НИКОГДА не упоминать руки — даже в негативе.** «no hands / deformed hands» —
   ловушка внимания, модель начинает лезть руками в кадр ИМЕННО из-за этого.
   Вместо этого не описывать действий-«handling» (вынимает, нажимает, держит и
   вертит) — движение отдавать камере или предмету.
2. **Одно доминирующее движение на кадр.** Два = хаос на скорости ленты.
3. **QC последних 2 секунд** — там вылезают посторонние объекты и дрейф стиля.
   Нам нужны первые ~3 сек, конец режем не глядя.

**Общий негативный промпт** (без единого упоминания рук):
`talking, open mouth, speaking, text, letters, numbers, watermark, logo,
brand marks, user interface, screen content, cuts, transitions, scene change,
morphing, distortion, blurry, low quality`

💡 **Экономия кредитов:** Kling умеет image-to-video. Сгенерировать кадр картинкой
(бесплатно) и оживить его — дешевле и точнее, чем text-to-video: композиция
зафиксирована референсом, промахов меньше.

---

## Подтверждённые (ядро первой волны)

### H1. Айфон ниже рынка — товарный, график-демо
- **VO:** «Взял айфон на восемнадцать тысяч дешевле рынка. Без серых схем.»
- **Титр:** `71 305 ₽` ~~`89 990 ₽`~~ (влетает на 1.5с)
- **Визуал:** момент распаковки — человек достаёт новый телефон из коробки
- **Kling:** `A young man sits on the floor of a warm-lit apartment, an opened delivery box beside him, a new smartphone resting in his lap as he looks down at it with a quiet satisfied smile. Camera slow push-in. Handheld documentary style, natural evening window light through a window behind him, shallow depth of field, warm color grading, vertical 9:16, 1080p.`
- **Beauty-вставка:** `web/hero-iphone-turn.webm` — наш Blender-рендер, идёт ПОСЛЕ хука
- **Середина:** ChatAlertIphone (алерт → тап → график)

### H2. «Я больше не переплачиваю. Вообще»
- **VO:** «Я больше не переплачиваю. Вообще.»
- **Титр:** `−18 000 ₽` в этом месяце (или без цифры, если нет реальной)
- **Визуал:** телефон роняют экраном вниз на диван, рядом кофе — расслабленность
- **Kling:** `A smartphone lies face-down on a soft couch cushion next to a coffee mug, a blanket rumpled around it. Camera static, slow ambient drift of morning light across the fabric. Calm cozy living room, natural window light, cinematic warm color grading, shallow depth of field, vertical 9:16, 1080p.`
- **Середина:** любой alert-ролик

### H3. «Способ для ленивых» — развилка ×4 (общий / Ozon / WB / Я.Маркет)
- **VO:** «Как не переплачивать на ⟨WB и Озоне⟩ — способ для ленивых.»
- **Титр:** `СПОСОБ ДЛЯ ЛЕНИВЫХ` + логотип маркетплейса
- **Визуал:** человек падает на диван, телефон опускается на грудь — лень
- **Kling:** `A person sinks backwards into a deep sofa and settles motionless, framed from the chest down, a phone resting on the blanket beside them. Camera static, slight handheld sway. Lazy relaxed evening at home, warm lamp light from one side, cinematic shallow depth of field, muted cozy tones, vertical 9:16, 1080p.`
- **Развилка:** маркетплейс меняется в VO, титре (логотип) и продуктовом бите

### H4. «Я один устал проверять цены?»
- **VO:** «Я один захожу на ВБ по десять раз в день просто глянуть цену?»
- **Титр:** `ЗНАКОМО?`
- **Визуал:** лицо в свете экрана вечером, бесконечный скролл (= блок H4 реестра)
- **Kling:** `A young woman lies in bed at night, her face lit only by the glow of a phone screen below the frame, eyes tired and unfocused, blinking slowly. Camera very slow push-in on her face. Dark bedroom, single cold light source, moody teal-and-orange cinematic grading, shallow depth of field, vertical 9:16, 1080p.`
- **Середина:** search-ролик · тип: джеб (мягкий CTA)

### H5. «Товары надо покупать на падении»
- **VO:** «Товары надо покупать на падении. Вопрос — как его поймать.»
- **Титр:** `ПОКУПАЙ НА ДНЕ`
- **Визуал:** человек в магазине смотрит на ценник и кладёт товар обратно на полку
- **Kling:** `A shopper stands still in a bright electronics store aisle facing a shelf of boxed products, shoulders dropping in hesitation, then turns and walks away out of frame. Camera static, subtle handheld sway. Documentary style seen from behind, face not visible, even fluorescent store lighting, natural colors, vertical 9:16, 1080p.`
- **Середина:** график-ролик (показать волатильность)

### H6. «Когда цена упадёт?» (кликбейт → честное раскрытие)
- **VO хука:** «Когда цена упадёт — не знает никто.»
- **VO раскрытия (сразу после):** «Но можно узнать первым.»
- **Титр:** `КОГДА УПАДЁТ ЦЕНА?`
- **Визуал:** человек застыл в проходе магазина, смотрит в телефон, не решается
- **Kling:** `A man stands frozen in a supermarket aisle staring down at a phone below the frame, completely motionless while the blurred aisle stretches behind him. Camera very slow push-in. Tense indecisive mood, cool overhead store lighting, shallow depth of field, cinematic muted grading, vertical 9:16, 1080p.`
- **Середина:** alert или search

### H7. «Уведомление на снижение цены» — развилка ×3 (Ozon / WB / Я.Маркет)
- **VO:** «Уведомление, когда цена упадёт на ⟨Озоне⟩? Да, так можно.»
- **Титр:** `КАК ПОСТАВИТЬ УВЕДОМЛЕНИЕ О ЦЕНЕ` + логотип
- **Визуал:** телефон в темноте загорается уведомлением (блок H10)
- **Kling:** `A phone lying face-up on a wooden nightstand suddenly lights up in a pitch-dark bedroom, its glow spilling across the surface and the edge of a pillow. Camera static, macro framing. Night interior, single light source from the screen, deep shadows, cinematic atmospheric grading, shallow depth of field, vertical 9:16, 1080p.`
- **Заголовок площадки = дословный поисковый запрос, не менять.** Самый горячий интент.

### H8. «Когда будет скидка?» (кликбейт → раскрытие)
- **VO хука:** «Когда будет скидка — не знает даже продавец.»
- **VO раскрытия:** «Но можно её не пропустить.»
- **Титр:** `КОГДА БУДЕТ СКИДКА?`
- **Визуал:** человек у витрины с ярлыками распродажи, скептически щурится
- **Kling:** `A woman stands in front of a shop window plastered with red sale posters, arms crossed, narrowing her eyes skeptically at the display. Camera static, slight handheld sway. Street reflections in the glass, overcast daylight, documentary style seen from the side, natural desaturated colors, vertical 9:16, 1080p.`
- **Середина:** график-ролик (доказать, что «скидка» бывает ненастоящей)

---

## От друга (перекуп — для него «зарабатываю» честно)

### H9. «Закупаться на 1688 больше не модно»
- **VO:** «Закупаться на 1688 больше не модно.»
- **Титр:** `1688 — ВСЁ?`
- **Визуал:** гора одинаковых коробок, медленный наезд
- **Kling:** `Rows of identical plain cardboard boxes stacked high in a dim empty warehouse, dust drifting in the air. Camera slow dolly push-in down the aisle. Cold industrial lighting from above, long shadows, cinematic desaturated grading, deep perspective, vertical 9:16, 1080p.`
- ⚠️ Тренд-формат «X больше не модно». Аудитория — перекупы; держать вне общих
  волн или мерить отдельно.

### H10. «Как я зарабатываю на маркетплейсах» — развилка ×4
- **VO:** «Как я зарабатываю, перепродавая с ⟨Вайлдберриз⟩.»
- **Титр:** `ПЕРЕПРОДАЖА` + логотип
- **Визуал:** купюры веером на столе рядом с телефоном, наезд камеры
- **Kling:** `A fan of banknotes spread across a wooden table beside a smartphone and a small stack of parcels, seen from directly above. Camera slow push-in from top-down. Warm desk lamp light from one side, shallow depth of field, cinematic rich color grading, vertical 9:16, 1080p.`
- ⚠️ Только для reseller-аудитории; «зарабатываю» честно лишь в этом контексте.

---

## Пул кликбейта (тест во вторую очередь)

Структура та же (VO / титр / визуал / Kling-промпт допишем при отборе). Отобрать 2–3:

- **«Посмотри, что мне бот прислал в шесть утра»** — любопытство; хук = сам алерт.
- **«Не покупай ничего на WB, пока не сделаешь это»** — warning; «это» = отслеживание.
- **«Цена на этот товар менялась ⟨N⟩ раз за месяц»** — шок-факт из нашей БД.
- **«Самая дорогая привычка — покупать в день, когда захотел»** — contrarian.
- **«Проверять цены вручную больше не модно»** — тренд-формат друга, наш смысл.
- **«Сколько я сэкономил за месяц — сам в шоке»** — азарт заработка, честно про экономию (нужна реальная сумма).

---

## Как это масштабируется

Хук × товар × маркетплейс = матрица ремиксов. Меняются 3 слоя хука + продуктовый
бит в середине (данные диалога), endcard неизменен. Один хук-паттерн
(«Как купить ⟨товар⟩ ниже рынка») наполняется любым товаром — строчка данных,
а не пересъёмка.
