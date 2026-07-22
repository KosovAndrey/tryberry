// Фирменные токены — те же значения, что на tryberry.ru (web/index.html).
// Держим здесь, чтобы все ролики выглядели одним продуктом и совпадали с сайтом.
// Отличительные активы из канона §4 (цвет-акцент, шрифты) фиксируются тут.
export const BRAND = {
  berry: '#e8336c',
  bright: '#ff5d8f',
  deep: '#a01651',
  rose: '#ffd3e1',
  cream: '#fff7fb',
  ink: '#170b11',

  // Сцена
  bg: '#120a10', // фон чата, тёмная тема «смотрится дороже» (реестр ассетов)
  botBubble: '#241019',
  botText: '#ffe9f1',
  userText: '#ffffff',

  // Шрифты подгружаются в fonts.ts (Unbounded / Onest — как на сайте)
  radius: 34,
} as const;

// Формат вывода: «12 490 ₽» — узкий неразрывный пробел как разделитель разрядов.
export const rub = (n: number): string =>
  n.toLocaleString('ru-RU').replace(/ /g, ' ') + ' ₽';
