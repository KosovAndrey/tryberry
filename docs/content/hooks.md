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

**Общий негативный промпт** (один для всех хуков):
`talking, open mouth, speaking, deformed hands, extra fingers, text, letters,
numbers, watermark, logo, brand marks, user interface, screen content, cuts,
transitions, distortion, extra objects, blurry, low quality`

---

## Подтверждённые (ядро первой волны)

### H1. Айфон ниже рынка — товарный, график-демо
- **VO:** «Взял айфон на восемнадцать тысяч дешевле рынка. Без серых схем.»
- **Титр:** `71 305 ₽` ~~`89 990 ₽`~~ (влетает на 1.5с)
- **Визуал:** момент распаковки — человек достаёт новый телефон из коробки
- **Kling:** `Vertical 9:16. A young man sits on the floor of a warm-lit apartment with an opened delivery box in his lap, lifting a new smartphone out of the packaging. He turns it slowly in the light with a small satisfied smile, looking down at the device, never at the camera. Soft evening window light, shallow depth of field, natural documentary colors, slow gentle push-in, single continuous shot.`
- **Beauty-вставка:** `web/hero-iphone-turn.webm` — наш Blender-рендер, идёт ПОСЛЕ хука
- **Середина:** ChatAlertIphone (алерт → тап → график)

### H2. «Я больше не переплачиваю. Вообще»
- **VO:** «Я больше не переплачиваю. Вообще.»
- **Титр:** `−18 000 ₽` в этом месяце (или без цифры, если нет реальной)
- **Визуал:** телефон роняют экраном вниз на диван, рядом кофе — расслабленность
- **Kling:** `Vertical 9:16. A smartphone drops face-down onto a soft couch next to a coffee cup, morning light, calm cozy mood, slow gentle motion, cinematic, warm tones. Single continuous shot.`
- **Середина:** любой alert-ролик

### H3. «Способ для ленивых» — развилка ×4 (общий / Ozon / WB / Я.Маркет)
- **VO:** «Как не переплачивать на ⟨WB и Озоне⟩ — способ для ленивых.»
- **Титр:** `СПОСОБ ДЛЯ ЛЕНИВЫХ` + логотип маркетплейса
- **Визуал:** человек падает на диван, телефон опускается на грудь — лень
- **Kling:** `Vertical 9:16. A phone lowers onto someone's chest as they sink back into a sofa, lazy relaxed evening, warm lamp light, slow motion, cinematic, faceless framing from chest down. Single continuous shot.`
- **Развилка:** маркетплейс меняется в VO, титре (логотип) и продуктовом бите

### H4. «Я один устал проверять цены?»
- **VO:** «Я один захожу на ВБ по десять раз в день просто глянуть цену?»
- **Титр:** `ЗНАКОМО?`
- **Визуал:** лицо в свете экрана вечером, бесконечный скролл (= блок H4 реестра)
- **Kling:** `Vertical 9:16. A person lies in bed at night, face lit only by a phone screen, endless scrolling reflected in tired eyes, moody teal grading, cinematic. Single continuous shot.`
- **Середина:** search-ролик · тип: джеб (мягкий CTA)

### H5. «Товары надо покупать на падении»
- **VO:** «Товары надо покупать на падении. Вопрос — как его поймать.»
- **Титр:** `ПОКУПАЙ НА ДНЕ`
- **Визуал:** человек в магазине смотрит на ценник и кладёт товар обратно на полку
- **Kling:** `Vertical 9:16. A person in a bright electronics store picks up a boxed product, glances at the price label, hesitates and puts it back on the shelf, then walks out of frame. Handheld documentary style, natural store lighting, shot from behind and to the side, face not visible, single continuous shot.`
- **Середина:** график-ролик (показать волатильность)

### H6. «Когда цена упадёт?» (кликбейт → честное раскрытие)
- **VO хука:** «Когда цена упадёт — не знает никто.»
- **VO раскрытия (сразу после):** «Но можно узнать первым.»
- **Титр:** `КОГДА УПАДЁТ ЦЕНА?`
- **Визуал:** палец завис над кнопкой «Купить», нерешительность (блок H7)
- **Kling:** `Vertical 9:16. Extreme close-up of a thumb hovering hesitantly over a glowing phone button, blurred interface, tension, cinematic shallow focus. Single continuous shot.`
- **Середина:** alert или search

### H7. «Уведомление на снижение цены» — развилка ×3 (Ozon / WB / Я.Маркет)
- **VO:** «Уведомление, когда цена упадёт на ⟨Озоне⟩? Да, так можно.»
- **Титр:** `КАК ПОСТАВИТЬ УВЕДОМЛЕНИЕ О ЦЕНЕ` + логотип
- **Визуал:** телефон в темноте загорается уведомлением (блок H10)
- **Kling:** `Vertical 9:16. A phone on a nightstand lights up with a notification in a dark room, the glow spilling onto the surface, cinematic, atmospheric. Single continuous shot.`
- **Заголовок площадки = дословный поисковый запрос, не менять.** Самый горячий интент.

### H8. «Когда будет скидка?» (кликбейт → раскрытие)
- **VO хука:** «Когда будет скидка — не знает даже продавец.»
- **VO раскрытия:** «Но можно её не пропустить.»
- **Титр:** `КОГДА БУДЕТ СКИДКА?`
- **Визуал:** человек у витрины с ярлыками распродажи, скептически щурится
- **Kling:** `Vertical 9:16. A shopper stands in front of a shop window covered in red sale tags, tilts their head skeptically and narrows their eyes, arms crossed. Street reflection in the glass, overcast daylight, handheld documentary framing, shot from the side, single continuous shot.`
- **Середина:** график-ролик (доказать, что «скидка» бывает ненастоящей)

---

## От друга (перекуп — для него «зарабатываю» честно)

### H9. «Закупаться на 1688 больше не модно»
- **VO:** «Закупаться на 1688 больше не модно.»
- **Титр:** `1688 — ВСЁ?`
- **Визуал:** гора одинаковых коробок, медленный наезд
- **Kling:** `Vertical 9:16. Rows of identical plain cardboard boxes stacked in a dim warehouse, slow dolly push-in, cool light, cinematic, anonymous. Single continuous shot.`
- ⚠️ Тренд-формат «X больше не модно». Аудитория — перекупы; держать вне общих
  волн или мерить отдельно.

### H10. «Как я зарабатываю на маркетплейсах» — развилка ×4
- **VO:** «Как я зарабатываю, перепродавая с ⟨Вайлдберриз⟩.»
- **Титр:** `ПЕРЕПРОДАЖА` + логотип
- **Визуал:** руки считают купюры рядом с телефоном (без лиц)
- **Kling:** `Vertical 9:16. Banknotes being counted next to a smartphone on a table, top-down, warm light, cinematic, no faces. Single continuous shot.`
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
