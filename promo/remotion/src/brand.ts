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

// ── Кадр стыка с AI-хуком ────────────────────────────────────────────
// Свечение на пустой подложке чата. Держим ЧИСЛАМИ, а не готовой строкой CSS,
// потому что тот же градиент пересобирает `scripts/seam-frame.mjs` — генератор
// PNG для конечного кадра Kling (в WSL Chrome не поднимается, `remotion still`
// там не работает). Одни числа на оба пути = кадр стыка не может разъехаться.
// Канон: docs/content/hooks.md, «Конечный кадр — наш PNG».
export const SEAM_GLOW = {
  rx: 0.6, // радиус эллипса по X, доля ширины кадра
  ry: 0.4, // радиус по Y, доля высоты
  cx: 0.8, // центр по X, доля ширины
  cy: 0.0, // центр по Y, доля высоты
  alpha: 0x55 / 255, // непрозрачность BRAND.deep в центре свечения
  stop: 0.7, // доля радиуса, на которой свечение уходит в ноль
} as const;

export const seamGlowCss = (): string => {
  const {rx, ry, cx, cy, alpha, stop} = SEAM_GLOW;
  const pct = (v: number) => `${v * 100}%`;
  const hexA = Math.round(alpha * 255)
    .toString(16)
    .padStart(2, '0');
  return `radial-gradient(${pct(rx)} ${pct(ry)} at ${pct(cx)} ${pct(cy)}, ${BRAND.deep}${hexA}, transparent ${pct(stop)})`;
};

// Формат вывода: «12 490 ₽» — узкий неразрывный пробел как разделитель разрядов.
export const rub = (n: number): string =>
  n.toLocaleString('ru-RU').replace(/ /g, ' ') + ' ₽';
