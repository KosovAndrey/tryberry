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
// только перед алертом. Оба alert-ролика — график-демо (тап → график в чате).
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
      secondary: {label: '📈 График цены', pressAt: 7.8},
      img: 'products/iphone.png',
    },
    {
      kind: 'chart',
      at: 8.8,
      time: '17:49',
      caption: '📈 История цены · 90 дней',
      series: [87990, 87990, 86490, 88990, 85990, 83490, 82990, 86990, 88490, 87490, 89990, 86990, 71305],
      usual: 87000,
      min: 71305,
      note: 'Обычно ~87 000 ₽. Сейчас 71 305 ₽ — дешевле не было · Ozon',
    },
  ],
};

// ── A2. AirPods Pro 3, товарный алерт — НОВЫЙ ФОРМАТ (утро → вечер) ──
// Цифры от владельца: было ~18 тыс., уведомление 13 216 ₽, порог круглый 14 000.
// Вопрос cold open — паттерн «адресация сегмента» (у каждого из 4 роликов свой
// паттерн вопроса — мини-тест формулировок). Типология роликов: search-версии
// демонстрируют подписку на выдачу, alert-версия с графиком — фичу графика.
export const alertBuds: ChatData = {
  title: 'TryBerry',
  coldOpen: {sec: 1.5, text: 'Ждёшь, когда подешевеют AirPods?'},
  typingOnlyAlert: true,
  messages: [
    {kind: 'text', from: 'user', at: 0.2, time: '08:47', text: 'wildberries.ru/catalog/1187…'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 1.1,
      time: '08:47',
      text: '✅ Отслеживаю: Apple AirPods Pro 3\n17 990 ₽ · Wildberries',
      buttons: [['✓ 🔻 Любое снижение'], ['📉 Ниже цены', '％ Скидка %']],
      press: {row: 1, col: 0, at: 2.3},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 3.1,
      time: '08:48',
      text: '✅ Порог 14 000 ₽ установлен.\n📉 сообщу, когда цена опустится ниже',
    },
    {kind: 'daybreak', at: 4.3, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 5.6,
      time: '18:05',
      title: '🎯 Ниже твоего порога — 14 000 ₽',
      name: 'Apple AirPods Pro 3',
      was: 17990,
      now: 13216,
      buy: 'Купить на WB за 13 216 ₽ →',
      secondary: {label: '📈 График цены', pressAt: 7.8},
      img: 'products/airpods.png',
    },
    {
      kind: 'chart',
      at: 8.8,
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

// ── S2. AirPods Pro 3, подписка на ВЫДАЧУ — НОВЫЙ ФОРМАТ (утро → вечер) ──
export const searchBuds: ChatData = {
  title: 'TryBerry',
  coldOpen: {sec: 1.5, text: 'Устал проверять цены каждый день?'},
  typingOnlyAlert: true,
  messages: [
    {kind: 'text', from: 'user', at: 0.2, time: '09:14', text: 'wildberries.ru/…search=airpods pro 3'},
    {
      kind: 'buttons',
      from: 'bot',
      at: 1.1,
      time: '09:14',
      text: '🔎 Это поиск — слежу за ВСЕЙ выдачей,\nне за одним товаром.',
      buttons: [['🔻 Любое снижение'], ['📉 Ниже цены']],
      press: {row: 1, col: 0, at: 2.3},
    },
    {
      kind: 'text',
      from: 'bot',
      at: 3.1,
      time: '09:15',
      text: '✅ Порог 14 000 ₽ установлен.\n🔎 сообщу, когда в выдаче будет дешевле',
    },
    {kind: 'daybreak', at: 4.3, label: 'спустя несколько часов'},
    {
      kind: 'alert',
      at: 5.6,
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
