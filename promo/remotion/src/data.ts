import {ChatData} from './ChatScene';

// Демо-диалоги. Тексты бота, кнопки и структура алерта — РЕАЛЬНЫЕ, из кода:
// internal/telegram/track.go (trackTriggerKeyboard, promptManualTarget, порог),
// internal/telegram/search.go (стратегия поиска), internal/telegram/notifier.go
// («📉 Цена снизилась!», «Скидка: N ₽ (P%)», ссылка на товар в тексте,
// кнопка «📈 График цены»).
//
// Модели и фото — ⟨ПОДСТАВИТЬ⟩: финальные берём из shorts-candidates.sql,
// фото кладём в public/products/ (брендовый дуотон применится сам).
// Разрыв времени НАМЕРЕННО без конкретики («спустя несколько часов») — момент
// падения цены мы не контролируем, обещать «6 дней» или «час» нельзя.

// ── C1/C4: товар. Полный флоу: ссылка → стратегия → порог → [часы] → алерт ──
export const alertDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '10:21', text: 'wildberries.ru/catalog/2049…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '10:21',
      text: '✅ Отслеживаю: Наушники Sonic Air Pro 2\n12 990 ₽ · Wildberries\n\n🔻 уведомлю при любом снижении цены',
      buttons: [
        ['✓ 🔻 Любое снижение'],
        ['📉 Ниже цены', '％ Скидка %'],
        ['📈 График цены'],
      ],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '10:21',
      text: '💰 Введи целевую цену в рублях (например 1499).\nУведомлю, когда цена опустится до неё или ниже.',
    },
    // Порог круглый — как ставят живые люди.
    {kind: 'text', from: 'user', at: 7.2, time: '10:22', text: '10 000'},
    {
      kind: 'text',
      from: 'bot',
      at: 8.6,
      time: '10:22',
      text: '✅ Порог 10 000 ₽ установлен.\n📉 уведомлю, когда цена опустится ниже 10 000 ₽',
    },
    // Разрыв времени — сердце сообщения: дальше смотрит ОН, а не ты.
    {kind: 'daybreak', at: 10.6, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 12.2,
      time: '17:48',
      title: '📉 Цена снизилась!',
      name: 'Наушники Sonic Air Pro 2',
      was: 12990,
      now: 9490,
      link: 'wildberries.ru/catalog/2049…',
      // img: 'products/headphones.png',
    },
  ],
};

// ── C2 (ядро): поиск. Ссылка на ВЫДАЧУ → стратегия → [часы] → мульти-алерт ──
export const searchDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '19:04', text: 'wildberries.ru/…search=наушники беспроводные'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '19:04',
      text: '🔎 Поиск: «наушники беспроводные» — слежу за всей выдачей, не за одним товаром.\n\nЕсли подборка устраивает — выбери, как уведомлять, и я начну следить 👇',
      buttons: [['🔻 Любое снижение'], ['📉 Ниже цены']],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '19:04',
      text: '💰 Введи целевую цену в рублях (например 59990).\nУведомлю, когда найдётся товар дешевле.',
    },
    {kind: 'text', from: 'user', at: 7.2, time: '19:05', text: '5 000'},
    {kind: 'daybreak', at: 9.2, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 10.8,
      time: '23:17',
      title: '🔎 Найдено дешевле 5 000 ₽',
      name: 'Наушники Redmi Buds 6 Active', // ⟨ПОДСТАВИТЬ⟩ модель из выдачи
      was: 6490,
      now: 4290,
      link: 'wildberries.ru/catalog/1187…',
      items: [
        {name: 'Soundcore P3 by Anker', price: 4790},
        {name: 'QCY MeloBuds T13', price: 4990},
      ],
      // img: 'products/redmi-buds.png',
    },
  ],
};
