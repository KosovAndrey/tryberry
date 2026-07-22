import React from 'react';
import {
  AbsoluteFill,
  useCurrentFrame,
  useVideoConfig,
  spring,
  interpolate,
} from 'remotion';
import {BRAND, rub} from './brand';
import {display, body} from './fonts';

// ── Схема данных ролика ──────────────────────────────────────────────
// Один диалог = массив сообщений. `at` — секунда появления. Меняешь данные →
// перерендериваешь: тот же шаблон, любой сценарий. В этом вся переиспользуемость.
export type Msg =
  | {from: 'user' | 'bot'; kind: 'text'; text: string; at: number}
  | {
      // Денежный кадр: карточка алерта бота «было → стало, −N ₽».
      from: 'bot';
      kind: 'alert';
      name: string;
      was: number;
      now: number;
      at: number;
    };

export type ChatData = {
  title: string; // имя в шапке чата, напр. «TryBerry»
  messages: Msg[];
};

// ── Пузырь входящей анимации ─────────────────────────────────────────
const Appear: React.FC<{at: number; children: React.ReactNode; align: 'left' | 'right'}> = ({
  at,
  children,
  align,
}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const local = frame - at * fps;
  const s = spring({frame: local, fps, config: {damping: 18, mass: 0.6}});
  const y = interpolate(s, [0, 1], [40, 0]);
  const scale = interpolate(s, [0, 1], [0.9, 1]);
  return (
    <div
      style={{
        display: 'flex',
        justifyContent: align === 'right' ? 'flex-end' : 'flex-start',
        opacity: s,
        transform: `translateY(${y}px) scale(${scale})`,
        marginBottom: 22,
      }}
    >
      {children}
    </div>
  );
};

const TextBubble: React.FC<{m: Extract<Msg, {kind: 'text'}>}> = ({m}) => {
  const isUser = m.from === 'user';
  return (
    <Appear at={m.at} align={isUser ? 'right' : 'left'}>
      <div
        style={{
          maxWidth: '78%',
          padding: '26px 32px',
          borderRadius: BRAND.radius,
          borderBottomRightRadius: isUser ? 10 : BRAND.radius,
          borderBottomLeftRadius: isUser ? BRAND.radius : 10,
          background: isUser
            ? `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`
            : BRAND.botBubble,
          color: isUser ? BRAND.userText : BRAND.botText,
          fontFamily: body,
          fontSize: 40,
          lineHeight: 1.32,
          fontWeight: 500,
        }}
      >
        {m.text}
      </div>
    </Appear>
  );
};

const AlertCard: React.FC<{m: Extract<Msg, {kind: 'alert'}>}> = ({m}) => {
  const drop = m.was - m.now;
  return (
    <Appear at={m.at} align="left">
      <div
        style={{
          maxWidth: '86%',
          padding: 34,
          borderRadius: BRAND.radius,
          borderBottomLeftRadius: 10,
          background: BRAND.botBubble,
          border: `2px solid ${BRAND.deep}`,
          fontFamily: body,
          color: BRAND.botText,
        }}
      >
        <div style={{fontSize: 34, opacity: 0.85, marginBottom: 14}}>🔔 Цена упала</div>
        <div style={{fontSize: 38, fontWeight: 600, marginBottom: 22, lineHeight: 1.25}}>
          {m.name}
        </div>
        <div style={{fontSize: 36, opacity: 0.7, textDecoration: 'line-through', marginBottom: 6}}>
          {rub(m.was)}
        </div>
        <div style={{fontFamily: display, fontSize: 72, fontWeight: 700, color: BRAND.cream}}>
          {rub(m.now)}
        </div>
        <div
          style={{
            marginTop: 20,
            display: 'inline-block',
            padding: '12px 24px',
            borderRadius: 100,
            background: BRAND.berry,
            color: '#fff',
            fontFamily: display,
            fontSize: 40,
            fontWeight: 700,
          }}
        >
          −{rub(drop)}
        </div>
      </div>
    </Appear>
  );
};

const Header: React.FC<{title: string}> = ({title}) => (
  <div
    style={{
      display: 'flex',
      alignItems: 'center',
      gap: 20,
      padding: '40px 44px',
      borderBottom: `1px solid rgba(255,255,255,0.08)`,
    }}
  >
    <div
      style={{
        width: 72,
        height: 72,
        borderRadius: 22,
        background: `linear-gradient(135deg, ${BRAND.bright}, ${BRAND.deep})`,
        display: 'flex',
        alignItems: 'center',
        justifyContent: 'center',
        fontSize: 40,
      }}
    >
      🫐
    </div>
    <div>
      <div style={{fontFamily: display, fontWeight: 700, fontSize: 40, color: BRAND.cream}}>
        {title}
      </div>
      <div style={{fontFamily: body, fontSize: 28, color: BRAND.bright}}>в сети · следит за ценой</div>
    </div>
  </div>
);

export const ChatScene: React.FC<ChatData> = ({title, messages}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();
  const now = frame / fps;
  const visible = messages.filter((m) => m.at <= now);

  return (
    <AbsoluteFill style={{background: BRAND.bg}}>
      {/* фирменное свечение, как в hero */}
      <AbsoluteFill
        style={{
          background: `radial-gradient(60% 40% at 80% 0%, ${BRAND.deep}55, transparent 70%)`,
        }}
      />
      <AbsoluteFill style={{flexDirection: 'column'}}>
        <Header title={title} />
        <div style={{flex: 1, padding: '44px 40px', display: 'flex', flexDirection: 'column', justifyContent: 'flex-end'}}>
          {visible.map((m, i) =>
            m.kind === 'alert' ? (
              <AlertCard key={i} m={m} />
            ) : (
              <TextBubble key={i} m={m} />
            ),
          )}
        </div>
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
