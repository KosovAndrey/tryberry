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

// ── A1. iPhone — НОВЫЙ ФОРМАТ удержания (сравниваем с наушниками-старыми) ──
// Cold open (награда первой) → сжатый флоу без мёртвого времени → алерт.
// Одна мысль на ролик: график НЕ показываем (он в отдельном ролике). Typing
// только перед алертом. Зум забракован на рендере — камера не используется.
export const alertIphone: ChatData = {
  title: 'TryBerry',
  coldOpen: {sec: 1.5, text: 'Как не пропускать скидки?'},
  typingOnlyAlert: true,
  messages: [
    {kind: 'text', from: 'user', at: 0.2, time: '10:21', text: 'ozon.ru/product/iphone-17-pro-max-256…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 1.1,
      time: '10:21',
      text: '✅ Отслеживаю: iPhone 17 Pro Max, 256 ГБ, белый\n89 990 ₽ · Ozon',
      buttons: [['✓ 🔻 Любое снижение'], ['📉 Ниже цены', '％ Скидка %']],
      press: {row: 1, col: 0, at: 2.3},
    },
    // «Введи цену» + ввод юзера вырезаны: зритель достроит сам, инфы ноль.
    {
      kind: 'text',
      from: 'bot',
      at: 3.1,
      time: '10:22',
      text: '✅ Порог 75 000 ₽ установлен.\n📉 сообщу, когда цена опустится ниже',
    },
    {kind: 'daybreak', at: 4.3, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 5.6,
      time: '17:48',
      title: '🎯 Ниже твоего порога — 75 000 ₽',
      name: 'iPhone 17 Pro Max, 256 ГБ, белый',
      was: 89990,
      now: 71305,
      buy: 'Купить на Ozon за 71 305 ₽ →',
      img: 'products/iphone.png',
    },
  ],
};

// ── A2. AirPods Pro 3, товарный алерт (утро → вечер) ──
// Цифры от владельца: было ~18 тыс., уведомление 13 216 ₽, порог круглый 14 000.
export const alertBuds: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '08:47', text: 'wildberries.ru/catalog/1187…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '08:47',
      text: '✅ Отслеживаю: Apple AirPods Pro 3\n17 990 ₽ · Wildberries\n\n🔻 уведомлю при любом снижении цены',
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
    {kind: 'text', from: 'user', at: 7.2, time: '08:48', text: '14 000'},
    {
      kind: 'text',
      from: 'bot',
      at: 8.6,
      time: '08:48',
      text: '✅ Порог 14 000 ₽ установлен.\n📉 уведомлю, когда цена опустится ниже 14 000 ₽',
    },
    {kind: 'daybreak', at: 10.6, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 12.2,
      time: '18:05',
      title: '🎯 Ниже твоего порога — 14 000 ₽',
      name: 'Apple AirPods Pro 3',
      was: 17990,
      now: 13216,
      buy: 'Купить на WB за 13 216 ₽ →',
      secondary: {label: '📈 График цены', pressAt: 14.8},
      img: 'products/airpods.png',
    },
    {
      kind: 'chart',
      at: 16.0,
      time: '18:06',
      caption: '📈 История цены · 90 дней',
      series: [17990, 17990, 17490, 18490, 16990, 16490, 16990, 17490, 17990, 17490, 18990, 17490, 13216],
      usual: 17500,
      min: 13216,
      note: 'Обычно ~17 500 ₽. Сейчас 13 216 ₽ — дешевле не было · Wildberries',
    },
  ],
};

// ── S1. iPhone, подписка на ВЫДАЧУ — НОВЫЙ ФОРМАТ ──
export const searchIphone: ChatData = {
  title: 'TryBerry',
  coldOpen: {sec: 1.5, text: 'Не знаешь, где купить выгодно?'},
  typingOnlyAlert: true,
  messages: [
    {kind: 'text', from: 'user', at: 0.2, time: '11:03', text: 'ozon.ru/search/?text=iphone 17 pro max 256'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 1.1,
      time: '11:03',
      text: '🔎 Это поиск — слежу за ВСЕЙ выдачей,\nне за одним товаром.',
      buttons: [['🔻 Любое снижение'], ['📉 Ниже цены']],
      press: {row: 1, col: 0, at: 2.3},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 3.1,
      time: '11:04',
      text: '✅ Порог 75 000 ₽ установлен.\n🔎 сообщу, когда в выдаче будет дешевле',
    },
    {kind: 'daybreak', at: 4.3, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 5.6,
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
      img: 'products/iphone.png',
    },
  ],
};

// ── S2. AirPods Pro 3, подписка на ВЫДАЧУ (утро → вечер) ──
export const searchBuds: ChatData = {
  title: 'TryBerry',
  messages: [
    {kind: 'text', from: 'user', at: 0.5, time: '09:14', text: 'wildberries.ru/…search=airpods pro 3'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 2.4,
      time: '09:14',
      text: '🔎 Поиск: «airpods pro 3» — слежу за всей выдачей, не за одним товаром.\n\nЕсли подборка устраивает — выбери, как уведомлять, и я начну следить 👇',
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
    {kind: 'text', from: 'user', at: 7.2, time: '09:15', text: '14 000'},
    {kind: 'daybreak', at: 9.2, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 10.8,
      time: '18:12',
      title: '🔎 По запросу «airpods pro 3» подешевело 3 товара',
      name: 'Apple AirPods Pro 3',
      was: 17990,
      now: 13216,
      buy: 'Купить на WB за 13 216 ₽ →',
      secondary: {label: '🔎 Открыть выдачу'},
      itemsTitle: 'Ещё варианты',
      items: [
        {name: 'AirPods Pro 3 (USB-C)', price: 13490},
        {name: 'Наушники Apple AirPods Pro 3', price: 13790},
      ],
      img: 'products/airpods.png',
    },
  ],
};
