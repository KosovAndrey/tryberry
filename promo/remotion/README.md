# promo/remotion — продуктовые ассеты кодом

Стилизованный фирменный чат бота и карточки-алертов для промо-шортсов —
рендерятся из React/HTML, а не снимаются с экрана телефона. Приём тот же, что в
hero-чате сайта; палитра и шрифты совпадают с tryberry.ru.

Зачем так (канон `docs/PROMO-SHORTS-PLAN.md` §4): продукт нельзя генерить
диффузией (Kling/Seedance ломают UI и текст) и не хочется снимать вручную.
Код даёт чёткий интерфейс, брендовую консистентность, ноль риска личных данных
и переиспользуемый шаблон: один `ChatScene` + данные диалога = любой ролик.

## Красная линия

Контейнер стилизован, но **тексты и поведение бота — настоящие**. Не рисуем
кнопок и ответов, которых в боте нет. Цифры в алертах — из
`scripts/sql/shorts-candidates.sql`, не выдумывать.

## Установка

```bash
cd promo/remotion
npm install
```

Рендер использует headless-Chrome (Remotion скачивает сам). В WSL/минимальном
Linux ему не хватает системных библиотек — поставить один раз:

```bash
sudo apt-get update && sudo apt-get install -y \
  libnss3 libnspr4 libatk1.0-0 libatk-bridge2.0-0 libcups2 libdrm2 \
  libgbm1 libasound2 libpango-1.0-0 libcairo2 libxcomposite1 \
  libxdamage1 libxfixes3 libxrandr2 libxkbcommon0
```

(На нашей машине `ldd` жаловался только на `libnss3`/`libnspr4`/`libnssutil3` —
остальное в списке на всякий случай, чтобы Chrome не открыл вторую партию
недостающих либ.)

## Использование

```bash
npm run dev                              # Remotion Studio: превью в браузере
npm run render ChatAlert  out/alert.mp4  # денежный кадр C1/C4
npm run render ChatSearch out/search.mp4 # подписка на выдачу C2 (ядро)
```

Новый диалог: добавь запись в `src/data.ts` и композицию в `src/Root.tsx`.
Схема сообщения — в `src/ChatScene.tsx` (тип `Msg`): текст / карточка-алерт,
поле `at` = секунда появления.

## Структура

- `src/brand.ts` — фирменные токены (цвета/радиусы), совпадают с сайтом.
- `src/fonts.ts` — Unbounded + Onest через `@remotion/google-fonts`.
- `src/ChatScene.tsx` — переиспользуемый шаблон чата (шапка, пузыри, алерт-карточка).
- `src/data.ts` — диалоги (данные, не верстка).
- `src/Root.tsx` — композиции (один шаблон × разные данные).

Проверено: `npm run typecheck` и `npm run bundle` проходят. Рендер в MP4 ждёт
установки системных либ выше.
