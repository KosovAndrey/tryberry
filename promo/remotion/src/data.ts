import {ChatData} from './ChatScene';

// Демо-диалоги. ВАЖНО: тексты обязаны совпадать с тем, что бот шлёт на самом
// деле, — стилизуем контейнер, но не выдумываем поведение (канон честности).
// Цифры в алертах — из scripts/sql/shorts-candidates.sql, не выдумывать.

// C1/C4 — денежный кадр «кинул ссылку → пришёл алерт».
export const alertDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {from: 'user', kind: 'text', text: 'https://www.wildberries.ru/catalog/…', at: 0.4},
    {from: 'bot', kind: 'text', text: 'Слежу за ценой. Напишу, когда упадёт.', at: 1.6},
    {from: 'bot', kind: 'alert', name: 'Робот-пылесос ⟨модель⟩', was: 18990, now: 13490, at: 3.6},
  ],
};

// C2 — ядро: подписка на ВЫДАЧУ, а не на товар.
export const searchDemo: ChatData = {
  title: 'TryBerry',
  messages: [
    {from: 'user', kind: 'text', text: 'Ссылка на поиск: «наушники беспроводные»', at: 0.4},
    {from: 'bot', kind: 'text', text: 'Слежу за всей выдачей — поймаю лучшую цену по запросу.', at: 1.8},
    {from: 'bot', kind: 'alert', name: 'Лучшее по запросу «наушники»', was: 6490, now: 4290, at: 3.8},
  ],
};
