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
      title: string; // «🔔 Цена упала» / «🔎 Найдено дешевле …»
      name: string;
      was: number;
      now: number;
      img?: string;
      items?: {name: string; price: number}[]; // хвост поисковой выдачи
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
      return 640 + (m.items?.length ?? 0) * 104;
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
        <span style={{whiteSpace: 'pre-line'}}>{m.text}</span>
        <Stamp time={m.time} dark={isUser} />
      </div>
    </Row>
  );
};

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
                const active = pressed && frame >= m.press!.at * fps;
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
  return (
    <Row align="left" enter={enter}>
      <div style={{position: 'relative', maxWidth: '92%'}}>
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
          <div style={{display: 'flex', justifyContent: 'space-between', alignItems: 'center', marginBottom: 20}}>
            <span style={{fontSize: 32, opacity: 0.9}}>{m.title}</span>
            <Stamp time={m.time} />
          </div>
          <div style={{display: 'flex', gap: 26, alignItems: 'center'}}>
            <ProductImage img={m.img} size={200} />
            <div>
              <div style={{fontSize: 36, fontWeight: 600, marginBottom: 14, lineHeight: 1.25}}>{m.name}</div>
              <div style={{fontSize: 32, opacity: 0.6, textDecoration: 'line-through'}}>{rub(m.was)}</div>
              <div style={{fontFamily: display, fontSize: 64, fontWeight: 700, color: BRAND.cream}}>
                {rub(m.now)}
              </div>
            </div>
          </div>
          <div
            style={{
              marginTop: 22,
              display: 'inline-block',
              padding: '12px 26px',
              borderRadius: 100,
              background: `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`,
              color: '#fff',
              fontFamily: display,
              fontSize: 38,
              fontWeight: 700,
            }}
          >
            −{rub(drop)}
          </div>
          {m.items && m.items.length > 0 && (
            <div style={{marginTop: 24, borderTop: '1px solid rgba(255,255,255,.10)', paddingTop: 18}}>
              {m.items.map((it, i) => (
                <div
                  key={i}
                  style={{display: 'flex', justifyContent: 'space-between', fontSize: 32, opacity: 0.75, marginBottom: 10}}
                >
                  <span style={{overflow: 'hidden', textOverflow: 'ellipsis', whiteSpace: 'nowrap', maxWidth: '68%'}}>
                    {it.name}
                  </span>
                  <span style={{fontWeight: 600}}>{rub(it.price)}</span>
                </div>
              ))}
            </div>
          )}
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
    (next.kind === 'alert' || next.from === 'bot') &&
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
              }
            })}
            {showTyping && <TypingBubble />}
          </div>
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
