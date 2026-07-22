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

## Рабочий цикл: рендер на WINDOWS (GoLand), не в WSL

⚠️ В WSL этой машины рендер НЕ работает: режим `networkingMode=mirrored`
ломает loopback (127.0.0.1 недостижим → Chrome DevTools не коннектится).
Решение: код правится где угодно, **рендер — из терминала GoLand на Windows**
(та же схема, что с Blender для hero-айфона). На Windows нужен Node LTS
(https://nodejs.org, winget: `winget install OpenJS.NodeJS.LTS`).

```powershell
cd promo\remotion
npm install          # разово; headless-Chrome Remotion скачает сам
npm run dev          # Remotion Studio: живое превью в браузере, тут смотрим и крутим
npx remotion render ChatAlertIphone out/alert-iphone.mp4   # товарный алерт + график
npx remotion render ChatSearchIphone out/search-iphone.mp4 # подписка на выдачу (ядро)
# также: ChatAlertBuds / ChatSearchBuds / Endcard
```

`npm run dev` — самый удобный вход: открывает Studio на localhost, там обе
композиции, скраббинг по таймлайну и правка props на лету. Сначала смотри там,
рендери когда картинка устроит.

`out/` и `node_modules/` в git не попадают (.gitignore) — готовые MP4 складывай
в `/промо/assets/` как остальные ассеты (реестр: docs/content/asset-library.md).

Новый диалог: добавь запись в `src/data.ts` и композицию в `src/Root.tsx`.
Схема сообщения — в `src/ChatScene.tsx` (тип `Msg`): текст / карточка-алерт,
поле `at` = секунда появления.

## Структура

- `src/brand.ts` — фирменные токены (цвета/радиусы), совпадают с сайтом.
- `src/fonts.ts` — Unbounded + Onest через `@remotion/google-fonts`.
- `src/ChatScene.tsx` — переиспользуемый шаблон чата (шапка, пузыри, алерт-карточка).
- `src/data.ts` — диалоги (данные, не верстка).
- `src/Root.tsx` — композиции (один шаблон × разные данные).

Проверено в WSL: `npm run typecheck` и `npm run bundle` проходят. Рендер в MP4 —
только с Windows-стороны (loopback, см. выше).
