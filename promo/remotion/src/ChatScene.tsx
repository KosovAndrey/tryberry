import React from 'react';
import {
  AbsoluteFill,
  Img,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
  spring,
  interpolate,
} from 'remotion';
import {BRAND, rub} from './brand';
import {display, body} from './fonts';

// ── Схема данных ролика ──────────────────────────────────────────────
// Диалог = массив сообщений, `at` — секунда появления. Тексты и кнопки ОБЯЗАНЫ
// совпадать с реальными (internal/telegram/track.go, search.go) — стилизуем
// контейнер, не выдумываем поведение.
export type Msg =
  | {kind: 'text'; from: 'user' | 'bot'; text: string; at: number; time?: string}
  | {
      // Сообщение бота с инлайн-кнопками (выбор стратегии отслеживания).
      kind: 'buttons';
      from: 'bot';
      text: string;
      at: number;
      time?: string;
      buttons: string[][]; // ряды кнопок, как в Telegram
      press?: {row: number; col: number; at: number}; // подсветка нажатия
    }
  | {
      // Разрыв времени: главный носитель смысла «вставил и забыл — он смотрит сам».
      kind: 'daybreak';
      label: string; // «спустя 6 дней · 134 проверки цены»
      at: number;
    }
  | {
      // Денежный кадр: алерт «было → стало». img — стилизованное фото товара
      // (staticFile из public/), без него рисуется брендовый глиф.
      kind: 'alert';
      at: number;
      time?: string;
      title: string; // реальный: «📉 Цена снизилась!» (notifier.go)
      name: string;
      was: number;
      now: number;
      img?: string;
      buy?: string; // текст CTA-кнопки, как в hero: «Купить на Ozon за 71 305 ₽ →»
      // Поисковая выдача: та же модель у других продавцов (названия чуть
      // отличаются, суть одна), все удовлетворяют порог, чуть дороже hero.
      itemsTitle?: string;
      items?: {name: string; price: number}[];
      chartPressAt?: number; // сек: анимация нажатия «📈 График цены»
    }
  | {
      // Упрощённый график истории цены — как на /p/, но в пузыре чата.
      // Цвета = сайт (chart.js): линия berry, опорные good/gold пунктиром
      // с подписями (не только цветом — подписи обязательны).
      kind: 'chart';
      at: number;
      time?: string;
      series: number[]; // цены по времени, последняя = текущая
      min: number; // опорная «минимум» (good)
      usual: number; // опорная «обычная» (gold)
      caption: string; // «История цены · 90 дней»
      note?: string; // подпись под графиком, как в hero: «Обычно ~87 000 ₽…»
    };

export type ChatData = {title: string; messages: Msg[]};

// Оценка высоты сообщения — для плавного сдвига ленты вверх (реальную высоту
// до рендера не узнать; на глаз с запасом, расхождение съедает пружина).
const estHeight = (m: Msg): number => {
  switch (m.kind) {
    case 'text':
      return 130 + Math.ceil(m.text.length / 34) * 46;
    case 'buttons':
      return 240 + m.buttons.length * 96;
    case 'daybreak':
      return 110;
    case 'alert':
      return 800 + (m.items?.length ?? 0) * 104;
    case 'chart':
      return 560;
  }
};

const TYPING_SEC = 0.9; // «печатает…» перед сообщениями бота

// ── Появление с пружиной ─────────────────────────────────────────────
const useEnter = (at: number) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  return spring({frame: frame - at * fps, fps, config: {damping: 16, mass: 0.6}});
};

const Row: React.FC<{align: 'left' | 'right' | 'center'; enter: number; children: React.ReactNode}> = ({
  align,
  enter,
  children,
}) => (
  <div
    style={{
      display: 'flex',
      justifyContent: align === 'right' ? 'flex-end' : align === 'center' ? 'center' : 'flex-start',
      opacity: enter,
      transform: `translateY(${interpolate(enter, [0, 1], [36, 0])}px)`,
      marginBottom: 24,
    }}
  >
    {children}
  </div>
);

const Stamp: React.FC<{time?: string; dark?: boolean}> = ({time, dark}) =>
  time ? (
    <span
      style={{
        fontFamily: body,
        fontSize: 26,
        color: dark ? 'rgba(255,255,255,.55)' : 'rgba(255,233,241,.45)',
        alignSelf: 'flex-end',
        marginLeft: 18,
        whiteSpace: 'nowrap',
      }}
    >
      {time}
    </span>
  ) : null;

const bubbleBase: React.CSSProperties = {
  maxWidth: '80%',
  padding: '24px 30px',
  borderRadius: BRAND.radius,
  fontFamily: body,
  fontSize: 38,
  lineHeight: 1.32,
  fontWeight: 500,
  display: 'flex',
  alignItems: 'flex-end',
};

const TextBubble: React.FC<{m: Extract<Msg, {kind: 'text'}>}> = ({m}) => {
  const enter = useEnter(m.at);
  const isUser = m.from === 'user';
  // Ссылки выглядят ссылками — подчёркивание, как в настоящем мессенджере.
  const isLink = /^https?:\/\//i.test(m.text) || /\.[a-z]{2}\//i.test(m.text);
  return (
    <Row align={isUser ? 'right' : 'left'} enter={enter}>
      <div
        style={{
          ...bubbleBase,
          borderBottomRightRadius: isUser ? 10 : BRAND.radius,
          borderBottomLeftRadius: isUser ? BRAND.radius : 10,
          background: isUser ? `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})` : BRAND.botBubble,
          color: isUser ? BRAND.userText : BRAND.botText,
        }}
      >
        <span
          style={{
            whiteSpace: 'pre-line',
            ...(isLink ? {textDecoration: 'underline', wordBreak: 'break-all', textUnderlineOffset: 6} : null),
          }}
        >
          {m.text}
        </span>
        <Stamp time={m.time} dark={isUser} />
      </div>
    </Row>
  );
};

// Имитация тапа: кнопка подсвечивается и ОТПУСКАЕТСЯ — возвращается как была.
const PRESS_HOLD = 0.55; // сек подсветки

const ButtonsBubble: React.FC<{m: Extract<Msg, {kind: 'buttons'}>}> = ({m}) => {
  const enter = useEnter(m.at);
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  return (
    <Row align="left" enter={enter}>
      <div style={{maxWidth: '86%'}}>
        <div
          style={{
            ...bubbleBase,
            maxWidth: '100%',
            borderBottomLeftRadius: 10,
            background: BRAND.botBubble,
            color: BRAND.botText,
          }}
        >
          <span style={{whiteSpace: 'pre-line'}}>{m.text}</span>
          <Stamp time={m.time} />
        </div>
        <div style={{marginTop: 14, display: 'flex', flexDirection: 'column', gap: 12}}>
          {m.buttons.map((row, r) => (
            <div key={r} style={{display: 'flex', gap: 12}}>
              {row.map((label, c) => {
                const pressed = m.press && m.press.row === r && m.press.col === c;
                const p = pressed
                  ? spring({frame: frame - m.press!.at * fps, fps, config: {damping: 12, mass: 0.5}})
                  : 0;
                const active =
                  pressed &&
                  frame >= m.press!.at * fps &&
                  frame < (m.press!.at + PRESS_HOLD) * fps;
                return (
                  <div
                    key={c}
                    style={{
                      flex: 1,
                      textAlign: 'center',
                      padding: '20px 14px',
                      borderRadius: 20,
                      border: `2px solid ${active ? BRAND.bright : 'rgba(255,255,255,.16)'}`,
                      background: active
                        ? `linear-gradient(120deg, ${BRAND.berry}, ${BRAND.deep})`
                        : 'rgba(255,255,255,.05)',
                      color: BRAND.cream,
                      fontFamily: body,
                      fontSize: 32,
                      fontWeight: 600,
                      whiteSpace: 'nowrap',
                      overflow: 'hidden',
                      transform: `scale(${1 + 0.06 * Math.sin(Math.min(p, 1) * Math.PI)})`,
                    }}
                  >
                    {label}
                  </div>
                );
              })}
            </div>
          ))}
        </div>
      </div>
    </Row>
  );
};

const Daybreak: React.FC<{m: Extract<Msg, {kind: 'daybreak'}>}> = ({m}) => {
  const enter = useEnter(m.at);
  return (
    <Row align="center" enter={enter}>
      <div
        style={{
          padding: '14px 34px',
          borderRadius: 100,
          background: 'rgba(255,255,255,.07)',
          border: '1px solid rgba(255,255,255,.10)',
          color: BRAND.rose,
          fontFamily: display,
          fontSize: 30,
          fontWeight: 500,
          letterSpacing: '.02em',
        }}
      >
        {m.label}
      </div>
    </Row>
  );
};

// Брендовый глиф товара (наушники) — когда нет фото. Дуотон под палитру.
const ProductGlyph: React.FC<{size: number}> = ({size}) => (
  <svg width={size} height={size} viewBox="0 0 100 100">
    <defs>
      <linearGradient id="pg" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0%" stopColor={BRAND.bright} />
        <stop offset="100%" stopColor={BRAND.deep} />
      </linearGradient>
    </defs>
    <path
      d="M20 62 v-8 a30 30 0 0 1 60 0 v8"
      fill="none"
      stroke="url(#pg)"
      strokeWidth="9"
      strokeLinecap="round"
    />
    <rect x="12" y="58" width="18" height="30" rx="9" fill="url(#pg)" />
    <rect x="70" y="58" width="18" height="30" rx="9" fill="url(#pg)" />
  </svg>
);

const ProductImage: React.FC<{img?: string; size: number}> = ({img, size}) => (
  <div
    style={{
      width: size,
      height: size,
      borderRadius: 24,
      background: `linear-gradient(135deg, ${BRAND.deep}33, ${BRAND.berry}22)`,
      border: `2px solid ${BRAND.deep}`,
      display: 'flex',
      alignItems: 'center',
      justifyContent: 'center',
      overflow: 'hidden',
      flexShrink: 0,
    }}
  >
    {img ? (
      // Дуотон: ч/б фото + брендовый градиент сверху — любое фото становится «нашим»
      <div style={{position: 'relative', width: '100%', height: '100%'}}>
        <Img
          src={staticFile(img)}
          style={{width: '100%', height: '100%', objectFit: 'cover', filter: 'grayscale(1) contrast(1.05)'}}
        />
        <div
          style={{
            position: 'absolute',
            inset: 0,
            background: `linear-gradient(135deg, ${BRAND.bright}55, ${BRAND.deep}66)`,
            mixBlendMode: 'color',
          }}
        />
      </div>
    ) : (
      <ProductGlyph size={size * 0.72} />
    )}
  </div>
);

const AlertCard: React.FC<{m: Extract<Msg, {kind: 'alert'}>}> = ({m}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  // Денежный кадр входит жёстче обычного пузыря + свечение за карточкой.
  const enter = spring({frame: frame - m.at * fps, fps, config: {damping: 11, mass: 0.7}});
  const glow = interpolate(frame - m.at * fps, [0, 12, 55], [0, 0.55, 0.18], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });
  const drop = m.was - m.now;
  const pct = Math.round((drop / m.was) * 100);
  return (
    <Row align="left" enter={enter}>
      <div style={{position: 'relative', maxWidth: '92%', width: '92%'}}>
        <div
          style={{
            position: 'absolute',
            inset: -40,
            background: `radial-gradient(50% 50% at 50% 50%, ${BRAND.berry}, transparent 70%)`,
            opacity: glow,
            filter: 'blur(30px)',
          }}
        />
        <div
          style={{
            position: 'relative',
            padding: 34,
            borderRadius: BRAND.radius,
            borderBottomLeftRadius: 10,
            background: BRAND.botBubble,
            border: `2px solid ${BRAND.berry}`,
            fontFamily: body,
            color: BRAND.botText,
          }}
        >
          <div style={{fontSize: 34, fontWeight: 600, marginBottom: 22}}>{m.title}</div>
          <div style={{display: 'flex', gap: 26, alignItems: 'center'}}>
            <ProductImage img={m.img} size={190} />
            <div style={{minWidth: 0}}>
              <div style={{fontSize: 36, fontWeight: 600, marginBottom: 12, lineHeight: 1.25}}>{m.name}</div>
              <div style={{display: 'flex', alignItems: 'baseline', gap: 18, flexWrap: 'wrap'}}>
                <span style={{fontSize: 32, opacity: 0.55, textDecoration: 'line-through'}}>{rub(m.was)}</span>
                <span style={{fontFamily: display, fontSize: 60, fontWeight: 700, color: BRAND.cream}}>
                  {rub(m.now)}
                </span>
              </div>
              <div
                style={{
                  marginTop: 14,
                  display: 'inline-block',
                  padding: '8px 22px',
                  borderRadius: 100,
                  background: `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`,
                  color: '#fff',
                  fontFamily: display,
                  fontSize: 32,
                  fontWeight: 700,
                }}
              >
                Скидка: {rub(drop)} (−{pct}%)
              </div>
            </div>
          </div>
          {/* CTA относятся к hero-товару и стоят СРАЗУ под ним, до списка
              вариантов. «Купить…» — как в hero-видео сайта; «📈 График цены» —
              реальная inline-кнопка (notifier.go priceAlertKeyboard). */}
          {m.buy && (
            <div style={{marginTop: 24, display: 'flex', flexDirection: 'column', gap: 12}}>
              <div
                style={{
                  textAlign: 'center',
                  padding: '22px 14px',
                  borderRadius: 22,
                  background: `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`,
                  color: '#fff',
                  fontFamily: body,
                  fontSize: 35,
                  fontWeight: 700,
                }}
              >
                {m.buy}
              </div>
              {(() => {
                // Тап по «График цены»: подсветка и отпускание (как живой палец)
                const p = m.chartPressAt
                  ? spring({frame: frame - m.chartPressAt * fps, fps, config: {damping: 12, mass: 0.5}})
                  : 0;
                const active =
                  m.chartPressAt !== undefined &&
                  frame >= m.chartPressAt * fps &&
                  frame < (m.chartPressAt + PRESS_HOLD) * fps;
                return (
                  <div
                    style={{
                      textAlign: 'center',
                      padding: '18px 14px',
                      borderRadius: 20,
                      border: `2px solid ${active ? BRAND.bright : 'rgba(255,255,255,.16)'}`,
                      background: active
                        ? `linear-gradient(120deg, ${BRAND.berry}, ${BRAND.deep})`
                        : 'rgba(255,255,255,.05)',
                      color: BRAND.cream,
                      fontFamily: body,
                      fontSize: 31,
                      fontWeight: 600,
                      transform: `scale(${1 + 0.06 * Math.sin(Math.min(p, 1) * Math.PI)})`,
                    }}
                  >
                    📈 График цены
                  </div>
                );
              })()}
            </div>
          )}
          {m.items && m.items.length > 0 && (
            <div style={{marginTop: 24, borderTop: '1px solid rgba(255,255,255,.10)', paddingTop: 16}}>
              <div style={{fontSize: 27, opacity: 0.5, marginBottom: 12, letterSpacing: '.03em'}}>
                {(m.itemsTitle ?? 'ещё варианты').toUpperCase()}
              </div>
              {m.items.map((it, i) => (
                <div
                  key={i}
                  style={{display: 'flex', justifyContent: 'space-between', fontSize: 31, opacity: 0.75, marginBottom: 10}}
                >
                  <span style={{overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '68%'}}>
                    {it.name}
                  </span>
                  <span style={{fontWeight: 600}}>{rub(it.price)}</span>
                </div>
              ))}
            </div>
          )}
          <div style={{display: 'flex', justifyContent: 'flex-end', marginTop: 14}}>
            <Stamp time={m.time} />
          </div>
        </div>
      </div>
    </Row>
  );
};

// Упрощённый график истории цены — оформление 1в1 с /p/ (web/assets/chart.js):
// линия BERRY c заливкой, опорные «минимум»(good)/«обычная»(gold) пунктиром,
// сетка едва заметная, маркер последней точки как cursor-point сайта.
const CHART = {
  berry: '#e8336c',
  berryHi: '#ff5d8f',
  grid: 'rgba(232,51,108,.10)',
  good: '#3fe0a8',
  gold: '#ffc24a',
  fill: 'rgba(232,51,108,.10)',
};

const ChartBubble: React.FC<{m: Extract<Msg, {kind: 'chart'}>}> = ({m}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const enter = useEnter(m.at);
  const local = frame - m.at * fps;
  // Линия отрисовывается слева направо ~1.4с, затем маркер и опорные.
  const draw = interpolate(local, [6, 48], [0, 1], {extrapolateLeft: 'clamp', extrapolateRight: 'clamp'});
  const refsIn = interpolate(local, [30, 52], [0, 1], {extrapolateLeft: 'clamp', extrapolateRight: 'clamp'});
  const markerIn = spring({frame: local - 46, fps, config: {damping: 10, mass: 0.5}});

  const W = 820;
  const H = 400;
  const PAD = {l: 26, r: 26, t: 30, b: 34};
  const lo = Math.min(...m.series, m.min) * 0.97;
  const hi = Math.max(...m.series, m.usual) * 1.04;
  const x = (i: number) => PAD.l + (i / (m.series.length - 1)) * (W - PAD.l - PAD.r);
  const y = (v: number) => PAD.t + (1 - (v - lo) / (hi - lo)) * (H - PAD.t - PAD.b);
  // Ступенчатый путь (step-after) — цена меняется РЕЗКО и держится уровнями,
  // как на реальном графике /p/ и в hero-видео. Никаких наклонных отрезков.
  let line = `M ${x(0)},${y(m.series[0])}`;
  for (let i = 1; i < m.series.length; i++) {
    line += ` H ${x(i)} V ${y(m.series[i])}`;
  }
  const area = `${line} V ${H - PAD.b} H ${x(0)} Z`;
  const last = m.series[m.series.length - 1];

  const refLine = (v: number, color: string, label: string) => (
    <>
      <line
        x1={PAD.l}
        y1={y(v)}
        x2={W - PAD.r}
        y2={y(v)}
        stroke={color}
        strokeWidth={2.5}
        strokeDasharray="12 10"
        opacity={refsIn * 0.75}
      />
      <text
        x={PAD.l + 4}
        y={y(v) - 10}
        fill={color}
        opacity={refsIn * 0.95}
        style={{fontFamily: body, fontSize: 24, fontWeight: 600}}
      >
        {label}
      </text>
    </>
  );

  return (
    <Row align="left" enter={enter}>
      <div
        style={{
          width: '92%',
          padding: '28px 24px 18px',
          borderRadius: BRAND.radius,
          borderBottomLeftRadius: 10,
          background: BRAND.botBubble,
          border: '1px solid rgba(232,51,108,.25)',
        }}
      >
        <div
          style={{
            fontFamily: body,
            fontSize: 31,
            fontWeight: 600,
            color: BRAND.botText,
            margin: '0 10px 16px',
          }}
        >
          {m.caption}
        </div>
        <svg width="100%" viewBox={`0 0 ${W} ${H}`}>
          {/* сетка — рецессивная, как на сайте */}
          {[0.25, 0.5, 0.75].map((t) => (
            <line
              key={t}
              x1={PAD.l}
              y1={PAD.t + t * (H - PAD.t - PAD.b)}
              x2={W - PAD.r}
              y2={PAD.t + t * (H - PAD.t - PAD.b)}
              stroke={CHART.grid}
              strokeWidth={1.5}
            />
          ))}
          <path d={area} fill={CHART.fill} opacity={draw} />
          <path
            d={line}
            fill="none"
            stroke={CHART.berry}
            strokeWidth={5}
            strokeLinejoin="round"
            strokeLinecap="round"
            pathLength={1}
            strokeDasharray={1}
            strokeDashoffset={1 - draw}
          />
          {refLine(m.usual, CHART.gold, `обычная ${rub(m.usual)}`)}
          {refLine(m.min, CHART.good, `минимум ${rub(m.min)}`)}
          {/* маркер последней цены — как cursor-point uPlot на сайте */}
          <circle
            cx={x(m.series.length - 1)}
            cy={y(last)}
            r={11 * Math.min(markerIn, 1.15)}
            fill={CHART.berryHi}
            stroke={BRAND.bg}
            strokeWidth={4}
          />
          <text
            x={x(m.series.length - 1) - 16}
            y={y(last) - 24}
            textAnchor="end"
            fill={BRAND.cream}
            opacity={Math.min(markerIn, 1)}
            style={{fontFamily: display, fontSize: 34, fontWeight: 700}}
          >
            {rub(last)}
          </text>
        </svg>
        {m.note && (
          <div
            style={{
              fontFamily: body,
              fontSize: 29,
              lineHeight: 1.4,
              color: 'rgba(255,233,241,.65)',
              margin: '10px 10px 0',
              opacity: refsIn,
            }}
          >
            {m.note}
          </div>
        )}
        <div style={{display: 'flex', justifyContent: 'flex-end', marginTop: 6, paddingRight: 10}}>
          <Stamp time={m.time} />
        </div>
      </div>
    </Row>
  );
};

// «Печатает…» перед сообщением бота
const TypingBubble: React.FC = () => {
  const frame = useCurrentFrame();
  return (
    <Row align="left" enter={1}>
      <div style={{...bubbleBase, background: BRAND.botBubble, borderBottomLeftRadius: 10, gap: 10, padding: '28px 32px'}}>
        {[0, 1, 2].map((i) => (
          <div
            key={i}
            style={{
              width: 14,
              height: 14,
              borderRadius: 7,
              background: BRAND.rose,
              opacity: 0.35 + 0.65 * Math.abs(Math.sin((frame / 9) - i * 0.9)),
            }}
          />
        ))}
      </div>
    </Row>
  );
};

const Header: React.FC<{title: string}> = ({title}) => (
  <div
    style={{
      display: 'flex',
      alignItems: 'center',
      gap: 22,
      padding: '42px 44px',
      borderBottom: '1px solid rgba(255,255,255,0.08)',
    }}
  >
    <Img src={staticFile('logo.png')} style={{width: 76, height: 76, borderRadius: 22}} />
    <div>
      <div style={{fontFamily: display, fontWeight: 700, fontSize: 40, color: BRAND.cream}}>{title}</div>
      <div style={{fontFamily: body, fontSize: 28, color: BRAND.bright}}>в сети · слежу за ценами</div>
    </div>
  </div>
);

export const ChatScene: React.FC<ChatData> = ({title, messages}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const now = frame / fps;

  const visible = messages.filter((m) => m.at <= now);
  // Типинг: перед ближайшим будущим сообщением бота (кроме daybreak/alert по вкусу
  // показываем и перед алертом — он тоже «приходит от бота»).
  const next = messages.find((m) => m.at > now);
  const showTyping =
    next &&
    next.kind !== 'daybreak' &&
    (next.kind === 'alert' || next.kind === 'chart' || next.from === 'bot') &&
    now >= next.at - TYPING_SEC;

  // Плавный сдвиг ленты: компенсируем мгновенный прыжок flex-end пружиной.
  let shift = 0;
  for (const m of visible) {
    const s = spring({frame: frame - m.at * fps, fps, config: {damping: 16, mass: 0.6}});
    shift += estHeight(m) * (1 - s);
  }

  return (
    <AbsoluteFill style={{background: BRAND.bg}}>
      <AbsoluteFill
        style={{background: `radial-gradient(60% 40% at 80% 0%, ${BRAND.deep}55, transparent 70%)`}}
      />
      <AbsoluteFill style={{flexDirection: 'column'}}>
        <Header title={title} />
        <div style={{flex: 1, overflow: 'hidden', display: 'flex', flexDirection: 'column', justifyContent: 'flex-end'}}>
          {/* нижний отступ 180 — сейф-зона под UI площадок (кнопки/подписи) */}
          <div style={{padding: '40px 40px 180px', transform: `translateY(${shift}px)`}}>
            {visible.map((m, i) => {
              switch (m.kind) {
                case 'text':
                  return <TextBubble key={i} m={m} />;
                case 'buttons':
                  return <ButtonsBubble key={i} m={m} />;
                case 'daybreak':
                  return <Daybreak key={i} m={m} />;
                case 'alert':
                  return <AlertCard key={i} m={m} />;
                case 'chart':
                  return <ChartBubble key={i} m={m} />;
              }
            })}
            {showTyping && <TypingBubble />}
          </div>
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
