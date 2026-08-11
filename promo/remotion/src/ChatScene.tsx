import React from 'react';
import {
  AbsoluteFill,
  Audio,
  Img,
  Sequence,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
  spring,
  interpolate,
} from 'remotion';
import {BRAND, rub, seamGlowCss} from './brand';
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
      // Вторичная кнопка: в товарном алерте «📈 График цены» (реальная кнопка
      // notifier.go), в поисковом «🔎 Открыть выдачу» (единственная реальная
      // кнопка searchAlertKeyboard). pressAt — симуляция тапа.
      secondary?: {label: string; pressAt?: number};
      // Остальные подешевевшие позиции выдачи (реальный алерт их перечисляет).
      itemsTitle?: string;
      items?: {name: string; price: number}[];
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

// Камера: ключи наездов (панчи на карточку/цену). Между ключами — пружина.
export type CamKey = {at: number; scale: number; y?: number};

export type ChatData = {
  title: string;
  messages: Msg[];
  // «Новый формат» удержания (см. batch-02 / фидбек друзей):
  // Cold open: вопрос-обращение к зрителю + алерт-награда СТАТИЧНО (без зума —
  // зум-версия забракована), потом вспышка и флоу.
  coldOpen?: {sec: number; text: string};
  camera?: CamKey[]; // зум-панчи; НЕ используются (забракованы), механика оставлена
  typingOnlyAlert?: boolean; // typing только перед алертом (минус мёртвое время)
  // Войсовер: фразы по битам, время АБСОЛЮТНОЕ (включая cold open) — в отличие
  // от messages.at. Файлы в public/vo/, генерация edge-tts (см. README).
  vo?: {at: number; src: string; vol?: number}[];
};

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

// Отступ слева у реплик БОТА (решение владельца 2026-08-02): юзер пишет от
// правого края как обычно, бот — не вплотную к левому. Так в кадре видно, что
// говорят двое, даже когда сообщения идут подряд. Карточки алерта и графика
// отступ НЕ получают: они шире пузырей по построению (92%), и сдвиг вывел бы
// их за кадр.
const BOT_INDENT = 44;

const Row: React.FC<{
  align: 'left' | 'right' | 'center';
  enter: number;
  indent?: boolean;
  children: React.ReactNode;
}> = ({align, enter, indent, children}) => (
  <div
    style={{
      display: 'flex',
      justifyContent: align === 'right' ? 'flex-end' : align === 'center' ? 'center' : 'flex-start',
      paddingLeft: indent && align === 'left' ? BOT_INDENT : 0,
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
    <Row align={isUser ? 'right' : 'left'} enter={enter} indent={!isUser}>
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
    <Row align="left" enter={enter} indent>
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

// Брендовый глиф товара — фолбэк, когда нет фото. Телефон или наушники —
// по названию товара (айфону наушники не рисуем).
const ProductGlyph: React.FC<{size: number; kind: 'phone' | 'buds'}> = ({size, kind}) => (
  <svg width={size} height={size} viewBox="0 0 100 100">
    <defs>
      <linearGradient id="pg" x1="0" y1="0" x2="1" y2="1">
        <stop offset="0%" stopColor={BRAND.bright} />
        <stop offset="100%" stopColor={BRAND.deep} />
      </linearGradient>
    </defs>
    {kind === 'phone' ? (
      <>
        <rect x="28" y="10" width="44" height="80" rx="10" fill="none" stroke="url(#pg)" strokeWidth="7" />
        <line x1="42" y1="20" x2="58" y2="20" stroke="url(#pg)" strokeWidth="6" strokeLinecap="round" />
        <circle cx="50" cy="79" r="4.5" fill="url(#pg)" />
      </>
    ) : (
      <>
        <path
          d="M20 62 v-8 a30 30 0 0 1 60 0 v8"
          fill="none"
          stroke="url(#pg)"
          strokeWidth="9"
          strokeLinecap="round"
        />
        <rect x="12" y="58" width="18" height="30" rx="9" fill="url(#pg)" />
        <rect x="70" y="58" width="18" height="30" rx="9" fill="url(#pg)" />
      </>
    )}
  </svg>
);

const ProductImage: React.FC<{img?: string; size: number; glyph: 'phone' | 'buds'}> = ({img, size, glyph}) => (
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
      <ProductGlyph size={size * 0.72} kind={glyph} />
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
          {/* Иерархия правлена 2026-08-11 под холодного зрителя (разбор
              retro-reel2-wb.md): цена и сумма экономии должны читаться с
              расстояния вытянутой руки за долю секунды, всё остальное —
              подпись к ним. До правки заголовок 34, название 36 и цена 60 были
              почти одного кегля, и глазу не за что было зацепиться. */}
          <div style={{fontSize: 30, fontWeight: 600, marginBottom: 20, opacity: 0.8}}>{m.title}</div>
          <div style={{display: 'flex', gap: 26, alignItems: 'center'}}>
            <ProductImage
              img={m.img}
              size={190}
              glyph={/iphone|телефон|смартфон/i.test(m.name) ? 'phone' : 'buds'}
            />
            <div style={{minWidth: 0}}>
              <div style={{fontSize: 32, fontWeight: 500, marginBottom: 10, lineHeight: 1.2, opacity: 0.75}}>
                {m.name}
              </div>
              <div style={{display: 'flex', alignItems: 'baseline', gap: 18, flexWrap: 'wrap'}}>
                <span style={{fontSize: 30, opacity: 0.5, textDecoration: 'line-through'}}>{rub(m.was)}</span>
                <span style={{fontFamily: display, fontSize: 78, fontWeight: 700, color: BRAND.cream, letterSpacing: -1}}>
                  {rub(m.now)}
                </span>
              </div>
              <div
                style={{
                  marginTop: 16,
                  display: 'inline-block',
                  padding: '10px 26px',
                  borderRadius: 100,
                  background: `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`,
                  color: '#fff',
                  fontFamily: display,
                  fontSize: 36,
                  fontWeight: 700,
                }}
              >
                −{rub(drop)} · {pct}%
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
              {m.secondary &&
                (() => {
                  const sec = m.secondary;
                  // Тап: подсветка и отпускание (как живой палец)
                  const p = sec.pressAt
                    ? spring({frame: frame - sec.pressAt * fps, fps, config: {damping: 12, mass: 0.5}})
                    : 0;
                  const active =
                    sec.pressAt !== undefined &&
                    frame >= sec.pressAt * fps &&
                    frame < (sec.pressAt + PRESS_HOLD) * fps;
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
                      {sec.label}
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
    <Row align="left" enter={1} indent>
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
      <div style={{display: 'flex', alignItems: 'center', gap: 12}}>
        {/* зелёная точка «онлайн» — как в настоящем мессенджере */}
        <div style={{width: 14, height: 14, borderRadius: 7, background: '#3fe0a8'}} />
        <span style={{fontFamily: body, fontSize: 28, color: BRAND.bright}}>в сети · слежу за ценами</span>
      </div>
    </div>
  </div>
);

// ── Кадр стыка с AI-хуком ────────────────────────────────────────────
// Пустая подложка чата = целевой конечный кадр, который отдаём Kling (канон:
// docs/content/hooks.md, «Конечный кадр — наш PNG»). Держим ОДНИМ компонентом,
// потому что его рендерят три разных места: фон самого чата, лид-ин перед
// первым сообщением и композиция SeamFrame для экспорта PNG. Разъедутся —
// конечный кадр хука перестанет совпадать с первым кадром середины, и весь
// смысл затеи со стыком пропадёт.
export const SeamBackdrop: React.FC = () => (
  <AbsoluteFill style={{background: BRAND.bg}}>
    <AbsoluteFill style={{background: seamGlowCss()}} />
  </AbsoluteFill>
);

// Обёртка: N кадров чистой подложки, затем обычный ChatScene. Sequence сдвигает
// таймбазу целиком, включая useCurrentFrame внутри вложенных компонентов и
// позиции звуковых Sequence — поэтому смещать вручную ничего не нужно.
export const ChatSceneLeadIn: React.FC<ChatData & {leadIn: number}> = ({leadIn, ...data}) => (
  <AbsoluteFill>
    <SeamBackdrop />
    <Sequence from={leadIn}>
      <ChatScene {...data} />
    </Sequence>
  </AbsoluteFill>
);

export const ChatScene: React.FC<ChatData> = ({title, messages, coldOpen, camera, typingOnlyAlert, vo}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();

  // Cold open: N секунд показываем НАГРАДУ (алерт) + вопрос зрителю, потом флоу.
  // Открытая петля: зритель уже видел деньги — досматривает, как к ним пришли.
  const coldFrames = (coldOpen?.sec ?? 0) * fps;
  const inColdOpen = coldFrames > 0 && frame < coldFrames;
  const chatFrame = Math.max(frame - coldFrames, 0);
  const now = chatFrame / fps;

  const alertMsg = messages.find((m): m is Extract<Msg, {kind: 'alert'}> => m.kind === 'alert');

  const visible = messages.filter((m) => m.at <= now);
  const next = messages.find((m) => m.at > now);
  const showTyping =
    next &&
    next.kind !== 'daybreak' &&
    (typingOnlyAlert
      ? next.kind === 'alert'
      : next.kind === 'alert' || next.kind === 'chart' || next.from === 'bot') &&
    now >= next.at - TYPING_SEC;

  // Плавный сдвиг ленты: компенсируем мгновенный прыжок flex-end пружиной.
  let shift = 0;
  for (const m of visible) {
    const s = spring({frame: chatFrame - m.at * fps, fps, config: {damping: 16, mass: 0.6}});
    shift += estHeight(m) * (1 - s);
  }

  // Камера: пружинный блендинг по ключам. Дрейф только вместе с ключами —
  // сам по себе на рендере смотрелся плохо (фидбек 2026-07-23).
  let camScale = 1;
  let camY = 0;
  for (const k of camera ?? []) {
    const s = spring({frame: chatFrame - k.at * fps, fps, config: {damping: 15, mass: 0.8}});
    camScale += (k.scale - camScale) * s;
    camY += ((k.y ?? 0) - camY) * s;
  }
  const drift = camera && camera.length > 0 ? 1 + 0.004 * now : 1;

  // ── Звук: МИНИМАЛЬНАЯ схема (решение 2026-07-23, зоопарк из 6 звуков
  // забракован). Два звука Mixkit: «Message pop alert» на каждое сообщение,
  // «Long pop» — ТОЛЬКО на финальное (график, а без него алерт). Финальный
  // звук = сигнатура завершения во всех роликах. Тапы/whoosh/ding выключены.
  const coldSec = coldOpen?.sec ?? 0;
  const sounds: {at: number; src: string; vol: number}[] = [];
  if (coldOpen) sounds.push({at: 0.05, src: 'sfx/pop-last.wav', vol: 0.8});
  const audible = messages.filter((m) => m.kind !== 'daybreak');
  const lastAudible = audible[audible.length - 1];
  for (const m of audible) {
    const isLast = m === lastAudible;
    sounds.push({
      at: coldSec + m.at,
      src: isLast ? 'sfx/pop-last.wav' : 'sfx/pop.wav',
      vol: isLast ? 0.85 : 0.6,
    });
  }

  // Вспышка на стыке cold open → чат
  const flash =
    coldFrames > 0
      ? interpolate(frame, [coldFrames - 3, coldFrames, coldFrames + 6], [0, 0.8, 0], {
          extrapolateLeft: 'clamp',
          extrapolateRight: 'clamp',
        })
      : 0;

  return (
    <AbsoluteFill>
      <SeamBackdrop />
      {inColdOpen && alertMsg && coldOpen ? (
        // ── Cold open: вопрос-обращение + алерт СТАТИЧНО (один вход, без зума) ──
        (() => {
          const qIn = spring({frame, fps, config: {damping: 14, mass: 0.6}});
          return (
            <AbsoluteFill style={{justifyContent: 'center', padding: '0 50px'}}>
              <div
                style={{
                  fontFamily: display,
                  fontWeight: 700,
                  fontSize: 72,
                  lineHeight: 1.15,
                  textAlign: 'center',
                  color: BRAND.cream,
                  marginBottom: 56,
                  opacity: qIn,
                  transform: `translateY(${interpolate(qIn, [0, 1], [24, 0])}px)`,
                }}
              >
                {coldOpen.text}
              </div>
              <div style={{transform: 'scale(1.06)'}}>
                <AlertCard m={{...alertMsg, at: 0.12, buy: undefined, secondary: undefined, items: undefined}} />
              </div>
            </AbsoluteFill>
          );
        })()
      ) : (
        <AbsoluteFill
          style={{
            flexDirection: 'column',
            transform: `scale(${camScale * drift}) translateY(${camY}px)`,
            transformOrigin: '50% 42%',
          }}
        >
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
      )}
      {flash > 0 && (
        <AbsoluteFill style={{background: BRAND.berry, opacity: flash, pointerEvents: 'none'}} />
      )}
      {sounds.map((s, i) => (
        <Sequence key={`snd-${i}`} from={Math.round(s.at * fps)}>
          <Audio src={staticFile(s.src)} volume={s.vol} />
        </Sequence>
      ))}
      {(vo ?? []).map((v, i) => (
        <Sequence key={`vo-${i}`} from={Math.round(v.at * fps)}>
          <Audio src={staticFile(v.src)} volume={v.vol ?? 1} />
        </Sequence>
      ))}
    </AbsoluteFill>
  );
};
