import {ChatData} from './ChatScene';

// Демо-диалоги. Тексты бота и кнопки — РЕАЛЬНЫЕ, из кода бота:
// internal/telegram/track.go (trackTriggerKeyboard, promptManualTarget,
// подтверждение порога) и internal/telegram/search.go (стратегия для поиска).
// Цифры товаров — ⟨ПОДСТАВИТЬ⟩ из scripts/sql/shorts-candidates.sql перед
// финальным рендером; текущие — вёрсточные болванки.

// ── C1/C4: товар. Полный флоу: ссылка → стратегия → порог → [дни] → алерт ──
export const alertDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '10:21', text: 'wildberries.ru/catalog/2049…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '10:21',
      text: '✅ Отслеживаю: Наушники Sonic Air Pro\n12 990 ₽ · Wildberries\n\n🔻 уведомлю при любом снижении цены',
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
    {kind: 'text', from: 'user', at: 7.2, time: '10:22', text: '9 990'},
    {
      kind: 'text',
      from: 'bot',
      at: 8.6,
      time: '10:22',
      text: '✅ Порог 9 990 ₽ установлен.\n📉 уведомлю, когда цена опустится ниже 9 990 ₽',
    },
    // Разрыв времени — сердце сообщения: дальше смотрит ОН, а не ты.
    {kind: 'daybreak', at: 10.6, label: 'спустя 6 дней · 134 проверки цены'},
    {
      kind: 'alert',
      at: 12.2,
      time: '09:12',
      title: '🔔 Цена упала',
      name: 'Наушники Sonic Air Pro',
      was: 12990,
      now: 9490,
      // img: 'products/headphones.png' — положить стилизованное фото в public/
    },
  ],
};

// ── C2 (ядро): поиск. Ссылка на ВЫДАЧУ → стратегия → [дни] → мульти-алерт ──
export const searchDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '19:04', text: 'wildberries.ru/…search=наушники беспроводные'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '19:04',
      text: '🔎 Это поиск, а не товар — слежу за ВСЕЙ выдачей.\n\nКак уведомлять о снижении цены?',
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
    {kind: 'daybreak', at: 9.2, label: 'спустя 3 дня · 87 проверок выдачи'},
    {
      kind: 'alert',
      at: 10.8,
      time: '08:40',
      title: '🔎 Найдено дешевле 5 000 ₽',
      name: 'TWS-наушники AirBuds S',
      was: 6490,
      now: 4290,
      items: [
        {name: 'Наушники Soundcore P3', price: 4790},
        {name: 'TWS Redmi Buds 5', price: 4990},
      ],
    },
  ],
};
