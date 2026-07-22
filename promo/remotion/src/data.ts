import {ChatData} from './ChatScene';

// 4 демо-диалога: 2 товара (iPhone из hero + наушники) × 2 сценария
// (товарный алерт с графиком / подписка на выдачу). Один шаблон ChatScene.
//
// Тексты бота, кнопки и структура — РЕАЛЬНЫЕ, из кода:
// - track.go: клавиатура стратегии, «💰 Введи целевую цену…», «✅ Порог…»
// - search.go: тот же паттерн для поиска
// - notifier.go: «🔎 По запросу «…» подешевело N товаров», кнопка
//   «🔎 Открыть выдачу» (единственная реальная кнопка поискового алерта),
//   «📈 График цены» у товарного.
// Оффер iPhone = hero-видео сайта (опубликованный канон). Наушники —
// ⟨ПОДСТАВИТЬ⟩ из shorts-candidates.sql перед финальным рендером.
// Времена во всех четырёх различаются; «спустя несколько часов» — без
// конкретики, момент падения цены мы не контролируем.

// ── A1. iPhone, товарный алерт: ссылка → стратегия → порог → алерт → график ──
export const alertIphone: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '10:21', text: 'ozon.ru/product/iphone-17-pro-max-256…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '10:21',
      text: '✅ Отслеживаю: iPhone 17 Pro Max, 256 ГБ, белый\n89 990 ₽ · Ozon\n\n🔻 уведомлю при любом снижении цены',
      buttons: [['✓ 🔻 Любое снижение'], ['📉 Ниже цены', '％ Скидка %'], ['📈 График цены']],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '10:21',
      text: '💰 Введи целевую цену в рублях (например 1499).\nУведомлю, когда цена опустится до неё или ниже.',
    },
    {kind: 'text', from: 'user', at: 7.2, time: '10:22', text: '75 000'},
    {
      kind: 'text',
      from: 'bot',
      at: 8.6,
      time: '10:22',
      text: '✅ Порог 75 000 ₽ установлен.\n📉 уведомлю, когда цена опустится ниже 75 000 ₽',
    },
    {kind: 'daybreak', at: 10.6, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 12.2,
      time: '17:48',
      title: '🎯 Ниже твоего порога — 75 000 ₽',
      name: 'iPhone 17 Pro Max, 256 ГБ, белый',
      was: 89990,
      now: 71305,
      buy: 'Купить на Ozon за 71 305 ₽ →',
      secondary: {label: '📈 График цены', pressAt: 14.8},
      // img: 'products/iphone.png',
    },
    {
      kind: 'chart',
      at: 16.0,
      time: '17:49',
      caption: '📈 История цены · 90 дней',
      series: [87990, 87990, 86490, 88990, 85990, 83490, 82990, 86990, 88490, 87490, 89990, 86990, 71305],
      usual: 87000,
      min: 71305,
      note: 'Обычно ~87 000 ₽. Сейчас 71 305 ₽ — дешевле не было · Ozon',
    },
  ],
};

// ── A2. Наушники, товарный алерт (утро → вечер) ──
export const alertBuds: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '08:47', text: 'wildberries.ru/catalog/1187…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '08:47',
      text: '✅ Отслеживаю: Наушники Redmi Buds 6 Active\n6 490 ₽ · Wildberries\n\n🔻 уведомлю при любом снижении цены',
      buttons: [['✓ 🔻 Любое снижение'], ['📉 Ниже цены', '％ Скидка %'], ['📈 График цены']],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '08:47',
      text: '💰 Введи целевую цену в рублях (например 1499).\nУведомлю, когда цена опустится до неё или ниже.',
    },
    {kind: 'text', from: 'user', at: 7.2, time: '08:48', text: '5 000'},
    {
      kind: 'text',
      from: 'bot',
      at: 8.6,
      time: '08:48',
      text: '✅ Порог 5 000 ₽ установлен.\n📉 уведомлю, когда цена опустится ниже 5 000 ₽',
    },
    {kind: 'daybreak', at: 10.6, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 12.2,
      time: '18:05',
      title: '🎯 Ниже твоего порога — 5 000 ₽',
      name: 'Наушники Redmi Buds 6 Active', // ⟨ПОДСТАВИТЬ⟩
      was: 6490,
      now: 4290,
      buy: 'Купить на WB за 4 290 ₽ →',
      secondary: {label: '📈 График цены', pressAt: 14.8},
      // img: 'products/buds.png',
    },
    {
      kind: 'chart',
      at: 16.0,
      time: '18:06',
      caption: '📈 История цены · 90 дней',
      series: [6290, 6290, 6090, 6490, 5990, 5790, 5990, 6290, 6490, 6290, 6690, 6390, 4290],
      usual: 6300,
      min: 4290,
      note: 'Обычно ~6 300 ₽. Сейчас 4 290 ₽ — дешевле не было · Wildberries',
    },
  ],
};

// ── S1. iPhone, подписка на ВЫДАЧУ ──
export const searchIphone: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '11:03', text: 'ozon.ru/search/?text=iphone 17 pro max 256'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '11:03',
      text: '🔎 Поиск: «iphone 17 pro max 256» — слежу за всей выдачей, не за одним товаром.\n\nЕсли подборка устраивает — выбери, как уведомлять, и я начну следить 👇',
      buttons: [['🔻 Любое снижение'], ['📉 Ниже цены']],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '11:03',
      text: '💰 Введи целевую цену в рублях (например 59990).\nУведомлю, когда найдётся товар дешевле.',
    },
    {kind: 'text', from: 'user', at: 7.2, time: '11:04', text: '75 000'},
    {kind: 'daybreak', at: 9.2, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 10.8,
      time: '18:26',
      title: '🔎 По запросу «iphone 17 pro max» подешевело 3 товара',
      name: 'iPhone 17 Pro Max, 256 ГБ, белый',
      was: 89990,
      now: 71305,
      buy: 'Купить на Ozon за 71 305 ₽ →',
      secondary: {label: '🔎 Открыть выдачу'},
      itemsTitle: 'Ещё варианты',
      items: [
        {name: 'iPhone 17 Pro Max 256 ГБ (ростест)', price: 72990},
        {name: 'Apple iPhone 17 Pro Max 256GB, White', price: 74490},
      ],
      // img: 'products/iphone.png',
    },
  ],
};

// ── S2. Наушники, подписка на ВЫДАЧУ (утро → вечер) ──
export const searchBuds: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '09:14', text: 'wildberries.ru/…search=наушники беспроводные'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '09:14',
      text: '🔎 Поиск: «наушники беспроводные» — слежу за всей выдачей, не за одним товаром.\n\nЕсли подборка устраивает — выбери, как уведомлять, и я начну следить 👇',
      buttons: [['🔻 Любое снижение'], ['📉 Ниже цены']],
      press: {row: 1, col: 0, at: 4.4},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 5.6,
      time: '09:14',
      text: '💰 Введи целевую цену в рублях (например 59990).\nУведомлю, когда найдётся товар дешевле.',
    },
    {kind: 'text', from: 'user', at: 7.2, time: '09:15', text: '5 000'},
    {kind: 'daybreak', at: 9.2, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 10.8,
      time: '18:12',
      title: '🔎 По запросу «наушники беспроводные» подешевело 3 товара',
      name: 'Наушники Redmi Buds 6 Active', // ⟨ПОДСТАВИТЬ⟩
      was: 6490,
      now: 4290,
      buy: 'Купить на WB за 4 290 ₽ →',
      secondary: {label: '🔎 Открыть выдачу'},
      itemsTitle: 'Ещё варианты',
      items: [
        {name: 'Наушники TWS Redmi Buds 6 Active', price: 4490},
        {name: 'Xiaomi Redmi Buds 6 Active, black', price: 4590},
      ],
      // img: 'products/buds.png',
    },
  ],
};
