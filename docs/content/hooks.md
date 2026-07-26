# Хуки для промо-роликов — рабочий каталог

Дата: 2026-07-24. Канон стратегии: [PROMO-SHORTS-PLAN.md](../PROMO-SHORTS-PLAN.md).
Структура ролика: **[AI-хук 2–3с] → [середина: бот, Remotion] → [endcard]**.
Хук = 3 слоя, которые НЕ дублируют друг друга (система из marketing-skills:ad-creative):

- **Визуал** — что в кадре (Veo/Kling, безликий, без текста) → стопит палец
- **VO** — первая произнесённая фраза → открывает петлю
- **Титр** — крупный текст на монтаже → работает для 85% зрителей без звука

Endcard во всех роликах один: логотип · @tryberrybot · «Проверять цены / вручную /
больше не нужно» · Telegram·VK·MAX · «Бесплатно. Без карты» · tryberry.ru.

Правило честности: заголовок-кликбейт снаружи допустим, только если середина
выполняет обещание. Цифры — из реальных данных, не выдумывать.

Veo-промпты: вертикаль 9:16, ~3 сек, без говорящих людей, БЕЗ текста в кадре,
движение принадлежит предметам. НЕ упоминать «руки/hands» (Veo сразу лезет лишними
руками, негатив делает хуже) — вместо этого «composition stays exactly as it is».

---

## Подтверждённые (ядро первой волны)

### H1. Айфон ниже рынка — товарный, график-демо
- **VO:** «Взял айфон на восемнадцать тысяч дешевле рынка. Без серых схем.»
- **Титр:** `71 305 ₽` ~~`89 990 ₽`~~ (влетает на 1.5с)
- **Визуал:** белый iPhone медленно вращается, блик скользит по корпусу
- **Veo:** `Vertical 9:16. A white smartphone slowly rotating in mid-air against a soft dark gradient background, cinematic studio light gliding across the glossy body, shallow depth of field, premium product feel. The composition stays exactly as it is. No text, no logos, no people.`
- **Середина:** ChatAlertIphone (алерт → тап → график)

### H2. «Я больше не переплачиваю. Вообще»
- **VO:** «Я больше не переплачиваю. Вообще.»
- **Титр:** `−18 000 ₽` в этом месяце (или без цифры, если нет реальной)
- **Визуал:** телефон роняют экраном вниз на диван, рядом кофе — расслабленность
- **Veo:** `Vertical 9:16. A smartphone drops face-down onto a soft couch next to a coffee cup, morning light, calm cozy mood, slow gentle motion, cinematic, warm tones. The composition stays exactly as it is. No text, no people faces.`
- **Середина:** любой alert-ролик

### H3. «Способ для ленивых» — развилка ×4 (общий / Ozon / WB / Я.Маркет)
- **VO:** «Как не переплачивать на ⟨WB и Озоне⟩ — способ для ленивых.»
- **Титр:** `СПОСОБ ДЛЯ ЛЕНИВЫХ` + логотип маркетплейса
- **Визуал:** человек падает на диван, телефон опускается на грудь — лень
- **Veo:** `Vertical 9:16. A phone lowers onto someone's chest as they sink back into a sofa, lazy relaxed evening, warm lamp light, slow motion, cinematic, faceless framing from chest down. The composition stays exactly as it is. No text.`
- **Развилка:** маркетплейс меняется в VO, титре (логотип) и продуктовом бите

### H4. «Я один устал проверять цены?»
- **VO:** «Я один захожу на ВБ по десять раз в день просто глянуть цену?»
- **Титр:** `ЗНАКОМО?`
- **Визуал:** лицо в свете экрана вечером, бесконечный скролл (= блок H4 реестра)
- **Veo:** `Vertical 9:16. A person lies in bed at night, face lit only by a phone screen, endless scrolling reflected in tired eyes, moody teal grading, cinematic. The composition stays exactly as it is. No text.`
- **Середина:** search-ролик · тип: джеб (мягкий CTA)

### H5. «Товары надо покупать на падении»
- **VO:** «Товары надо покупать на падении. Вопрос — как его поймать.»
- **Титр:** `ПОКУПАЙ НА ДНЕ`
- **Визуал:** коробка-посылка плавно опускается вниз сквозь кадр
- **Veo:** `Vertical 9:16. A plain cardboard parcel slowly floating downward through frame against a minimal gradient, soft light, weightless calm motion, cinematic. The composition stays exactly as it is. No text, no logos.`
- **Середина:** график-ролик (показать волатильность)

### H6. «Когда цена упадёт?» (кликбейт → честное раскрытие)
- **VO хука:** «Когда цена упадёт — не знает никто.»
- **VO раскрытия (сразу после):** «Но можно узнать первым.»
- **Титр:** `КОГДА УПАДЁТ ЦЕНА?`
- **Визуал:** палец завис над кнопкой «Купить», нерешительность (блок H7)
- **Veo:** `Vertical 9:16. Extreme close-up of a thumb hovering hesitantly over a glowing phone button, blurred interface, tension, cinematic shallow focus. The composition stays exactly as it is. No readable text.`
- **Середина:** alert или search

### H7. «Уведомление на снижение цены» — развилка ×3 (Ozon / WB / Я.Маркет)
- **VO:** «Уведомление, когда цена упадёт на ⟨Озоне⟩? Да, так можно.»
- **Титр:** `КАК ПОСТАВИТЬ УВЕДОМЛЕНИЕ О ЦЕНЕ` + логотип
- **Визуал:** телефон в темноте загорается уведомлением (блок H10)
- **Veo:** `Vertical 9:16. A phone on a nightstand lights up with a notification in a dark room, the glow spilling onto the surface, cinematic, atmospheric. The composition stays exactly as it is. No readable text.`
- **Заголовок площадки = дословный поисковый запрос, не менять.** Самый горячий интент.

### H8. «Когда будет скидка?» (кликбейт → раскрытие)
- **VO хука:** «Когда будет скидка — не знает даже продавец.»
- **VO раскрытия:** «Но можно её не пропустить.»
- **Титр:** `КОГДА БУДЕТ СКИДКА?`
- **Визуал:** красный ярлык-стикер скидки крупно, лёгкое покачивание
- **Veo:** `Vertical 9:16. A red discount price tag sticker swinging gently on a string against a clean background, soft light, macro, cinematic. The composition stays exactly as it is. No readable text.`
- **Середина:** график-ролик (доказать, что «скидка» бывает ненастоящей)

---

## От друга (перекуп — для него «зарабатываю» честно)

### H9. «Закупаться на 1688 больше не модно»
- **VO:** «Закупаться на 1688 больше не модно.»
- **Титр:** `1688 — ВСЁ?`
- **Визуал:** гора одинаковых коробок, медленный наезд
- **Veo:** `Vertical 9:16. Rows of identical plain cardboard boxes stacked in a dim warehouse, slow dolly push-in, cool light, cinematic, anonymous. The composition stays exactly as it is. No text, no logos.`
- ⚠️ Тренд-формат «X больше не модно». Аудитория — перекупы; держать вне общих
  волн или мерить отдельно.

### H10. «Как я зарабатываю на маркетплейсах» — развилка ×4
- **VO:** «Как я зарабатываю, перепродавая с ⟨Вайлдберриз⟩.»
- **Титр:** `ПЕРЕПРОДАЖА` + логотип
- **Визуал:** руки считают купюры рядом с телефоном (без лиц)
- **Veo:** `Vertical 9:16. Banknotes being counted next to a smartphone on a table, top-down, warm light, cinematic, no faces. The composition stays exactly as it is. No readable text.`
- ⚠️ Только для reseller-аудитории; «зарабатываю» честно лишь в этом контексте.

---

## Пул кликбейта (тест во вторую очередь)

Структура та же (VO / титр / визуал / Veo допишем при отборе). Отобрать 2–3:

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
