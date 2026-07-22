# Библиотека переиспользуемых видео-ассетов

Идея: демо бота и графики снимаем/рендерим **один раз**, хуки и обвязка — разные
в каждом ролике. Один ассет = один короткий клип 3–8 сек, вертикаль 1080×1920
(или 1170×2532 скринкаст с айфона), без вшитых субтитров — титры кладём на
монтаже. Хранение: `/промо/assets/` (локально, не в git), здесь — реестр.

Статус: ⬜ не записан / ✅ готов.

## A. Скринкасты бота (записать одной сессией, демо-аккаунт)

Как записывать: запись экрана телефона (iPhone: Настройки → Пункт управления →
Запись экрана) ИЛИ Telegram Desktop в узком окне + OBS. Тёмная тема (в кадре
смотрится дороже), чистый демо-чат без личных данных, до записи прогнать сцену
2 раза, чтобы не было пауз-раздумий. Печатать медленнее обычного — на монтаже
ускоряется красиво, обратное — нет.

| ID | Клип | Что в кадре (шаг за шагом) | Статус |
|---|---|---|---|
| A1 | «Кинул ссылку» | Копируем ссылку товара в приложении WB → переключаемся в TG → вставляем боту → отправить. **Записать 4 раза** — в WB / Ozon / Я.Маркет / Ali: батч-02 делает версии по маркетплейсам, и это единственное место, где съёмка умножается | ⬜ |
| A2 | «Бот подхватил» | Ответ бота: карточка товара, текущая цена, кнопки | ⬜ |
| A3 | «Алерт пришёл» | Пуш/сообщение «Цена упала: было X → стало Y, −N ₽» (на демо-товаре с реальным падением; можно поднять first_seen на стейдже, чтобы спровоцировать алерт) | ⬜ |
| A4 | «График в боте» | Открытие графика истории цены из сообщения бота | ⬜ |
| A5 | «Целевая цена» | Установка «сообщи, когда будет дешевле X» | ⬜ |
| A6 | «Поиск по ссылке» ⭐ | Кинули ссылку на ВЫДАЧУ (поиск WB/Ozon) → бот следит за лучшей ценой. **Ядро сообщения с батча-02** — единственная фича, которой нет у альтернатив; записать тщательнее остальных | ⬜ |
| A7 | «Снова в наличии» | Алерт back-in-stock | ⬜ |
| A8 | «Настроил маме» | Тот же A1–A2, но рука держит телефон с крупным шрифтом/светлая тема — «мамин телефон» | ⬜ |

## G. Рендеры графиков (кодом: страница /p/<id> или AE-шаблон)

Данные — реальные серии из нашей БД (история WB выгружается). Рендер: запись
страницы графика c плавным зумом ИЛИ AE-анимация по CSV. Один шаблон → десятки
товаров.

| ID | Клип | Содержание | Статус |
|---|---|---|---|
| G1 | «Фейковая скидка» | График 60–90 дней: линия цены ровная, в кадре ярлык «−60%» с карточки — стрелка показывает, что «старая цена» никогда не существовала | ⬜ |
| G2 | «Провал цены» | График с резким падением, маркер «здесь бот прислал алерт», подпись экономии в ₽ | ⬜ |
| G3 | «Качели» | Волатильный товар: цена гуляет ±30% за месяц — «покупать надо не когда нужно, а когда дёшево» | ⬜ |

## B. Брендовые блоки (сделать один раз в AE)

| ID | Клип | Содержание | Статус |
|---|---|---|---|
| B1 | CTA-эндкард | 3 сек: логотип, @tryberrybot, «первые 10 дней бесплатно», стрелка на описание | ⬜ |
| B2 | Подложка-титр | Анимированная плашка для текстов поверх футажа | ⬜ |
| B3 | Прайс-каунтер | Анимация цифр «4 720 ₽ → 2 890 ₽» (шаблон с подставляемыми числами) | ⬜ |

## H. AI-сцены (Seedance/Kling) — БЕЗЛИКИЙ переиспользуемый b-roll

Правило разделения (канон §4): диффузия трогает **только фон** — руки, предметы,
среда, эмоция без реплик. Она НЕ рисует наш интерфейс, НЕ показывает текст в
кадре, НЕ ведёт одного персонажа через ролик (лицо «плывёт» между генерациями).
Продукт = Remotion-рендер (блок P) + график-страница; текст = титры на монтаже.

Генерим по одному разу 5-сек клипы, складываем сюда, режем во все батчи. Один
клип закрывает хук ИЛИ связку в десятках роликов — меняются титры и порядок.
Все промпты: вертикаль 9:16, 5 сек, без говорящих ртов, без читаемого текста.

| ID | Клип | EN-промпт | Куда | Статус |
|---|---|---|---|---|
| H1 | Руки с телефоном, скролл ленты | `Vertical 9:16. Close-up of hands scrolling endlessly on a smartphone, screen glow on fingers, dim room, cinematic, shallow depth of field, no readable text on screen.` | C1, C2, джебы | ⬜ |
| H2 | Посылка на столе, распаковка | `Vertical 9:16. Hands unboxing a plain cardboard parcel on a table, top-down, soft natural light, documentary style, no logos, no text.` | C3, C4 | ⬜ |
| H3 | Телефон экраном вниз + кофе | `Vertical 9:16. A phone lies face down next to a coffee cup, morning light, a person moves in soft-focus background, calm, warm tones, no text.` | джеб «не проверяю цены» | ⬜ |
| H4 | Лицо в свете экрана, вечер | `Vertical 9:16. A person lies in bed at night, face lit only by a phone screen, slow zoom on tired eyes, moody teal grading, no text.` | C1, C5 | ⬜ |
| H5 | Календарь, отрывают дни | `Vertical 9:16. Calendar pages being torn off rapidly, dramatic side light, shallow depth of field, cinematic, no readable text.` | C1, C5 | ⬜ |
| H6 | Полки склада, ряды коробок | `Vertical 9:16. Slow dolly through warehouse shelves stacked with plain boxes, cool light, cinematic, anonymous, no logos or text.` | C4, C1 | ⬜ |
| H7 | Палец завис над «Купить» | `Vertical 9:16. Extreme close-up of a thumb hovering hesitantly over a phone screen button, blurred UI, tension, cinematic, no readable text.` | C3, C5 | ⬜ |
| H8 | Корзина вещей на столе | `Vertical 9:16. Top-down of assorted everyday products laid out on a table being sorted into two piles by hands, soft light, no logos, no text.` | C3 | ⬜ |
| H9 | Человек со спины у окна | `Vertical 9:16. A person seen from behind looking out a window holding a phone, natural backlight, contemplative, cinematic, faceless, no text.` | джебы, C5 | ⬜ |
| H10 | Уведомление светит в темноте | `Vertical 9:16. A phone on a nightstand lights up with a notification in a dark room, glow spills onto the table, cinematic, no readable text.` | C1, C4 | ⬜ |
| H11 | Много открытых вкладок | `Vertical 9:16. Fast cuts of switching between many browser tabs and apps on a phone, accelerating, motion blur, no readable text.` | C2, джебы | ⬜ |
| H12 | Руки считают на калькуляторе | `Vertical 9:16. Hands tapping numbers on a calculator next to a phone and receipts, top-down, warm light, no readable digits.` | C6, C1 | ⬜ |
| H13 | Кофейня, кто-то смотрит в телефон | `Vertical 9:16. A person at a bright cafe looking at a phone, expression shifting, slow push-in, natural window light, no text.` | C2, джебы | ⬜ |
| H14 | Смахивающий монтаж витрины | `Vertical 9:16. Abstract swipe transitions over blurred product-grid colors and price tags, fast rhythmic motion, no readable text.` | связки везде | ⬜ |
| H15 | Спокойные руки, чай, покой | `Vertical 9:16. Hands wrapped around a warm mug, unhurried, soft home light, phone untouched on the table, cozy, cinematic, no text.` | джеб «живу спокойно» | ⬜ |

Ещё 5 слотов (H16–H20) держим под удачные хуки из будущих батчей.

## P. Продукт кодом — Remotion (`promo/remotion/`)

Стилизованный фирменный чат бота и карточки-алертов, отрендеренные из HTML/React,
а не снятые с экрана. База — тот же приём, что в hero-чате сайта; палитра и
шрифты совпадают с tryberry.ru (`brand.ts`). Меняешь данные диалога (`src/data.ts`)
→ перерендериваешь: один шаблон = любой диалог. Держит красную линию — контейнер
стилизован, но тексты и поведение бота настоящие.

| ID | Композиция | Содержание | Статус |
|---|---|---|---|
| P1 | `ChatAlert` | «кинул ссылку → пришёл алерт было→стало −N₽» (C1/C4) | ✅ шаблон |
| P2 | `ChatSearch` | подписка на ВЫДАЧУ, не на товар (C2, ядро) | ✅ шаблон |

Рендер: `npm run render <id> out/<id>.mp4` (нужен headless-Chrome, разовая
установка системных либ — см. `promo/remotion/README.md`). График остаётся
отдельным ассетом (блок G) — он уже страница `/p/<public_id>`.

## Сборка (Premiere/AE/CapCut)

- Проект-шаблон Premiere: дорожки [хук][тело из ассетов][B1-эндкард], пресет
  9:16 1080×1920, экспорт H.264 ≤60 сек.
- Субтитры: CapCut Pro авто-сабы по-русски → стиль один раз настроить, сохранить
  как пресет. Либо whisper → SRT → Premiere.
- На скринкастах: зум-панч на ключевые элементы (цена, кнопка), лёгкое движение
  всегда — статичный скрин дольше 2 сек не держит внимание.
