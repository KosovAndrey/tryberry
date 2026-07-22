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
import {BRAND} from './brand';
import {display, body} from './fonts';

// B1 — CTA-эндкард, последние 3–4 сек каждого ролика. Отличительный актив:
// кадр в кадр одинаковый во всех роликах (канон §4), меняться не должен.
export const Endcard: React.FC = () => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();

  const logoIn = spring({frame, fps, config: {damping: 12, mass: 0.7}});
  const titleIn = spring({frame: frame - 8, fps, config: {damping: 14}});
  const pillIn = spring({frame: frame - 18, fps, config: {damping: 14}});
  const arrowBob = Math.sin(frame / 9) * 10;

  return (
    <AbsoluteFill
      style={{
        background: BRAND.bg,
        alignItems: 'center',
        justifyContent: 'center',
        flexDirection: 'column',
      }}
    >
      <AbsoluteFill
        style={{background: `radial-gradient(55% 40% at 50% 30%, ${BRAND.deep}66, transparent 70%)`}}
      />
      <div style={{position: 'relative', textAlign: 'center'}}>
        <Img
          src={staticFile('logo.png')}
          style={{
            width: 220,
            height: 220,
            borderRadius: 56,
            transform: `scale(${logoIn})`,
          }}
        />
        <div
          style={{
            fontFamily: display,
            fontWeight: 700,
            fontSize: 76,
            color: BRAND.cream,
            marginTop: 40,
            opacity: titleIn,
            transform: `translateY(${interpolate(titleIn, [0, 1], [30, 0])}px)`,
          }}
        >
          @tryberrybot
        </div>
        <div
          style={{
            fontFamily: body,
            fontSize: 44,
            color: BRAND.rose,
            marginTop: 18,
            opacity: titleIn,
          }}
        >
          Цену проверяет он, а не ты
        </div>
        {/* Три мессенджера — как ряд бейджей в hero сайта */}
        <div
          style={{
            display: 'flex',
            justifyContent: 'center',
            gap: 20,
            marginTop: 40,
            opacity: titleIn,
          }}
        >
          {['Telegram', 'VK', 'MAX'].map((m) => (
            <div
              key={m}
              style={{
                padding: '14px 34px',
                borderRadius: 100,
                border: '2px solid rgba(255,255,255,.18)',
                background: 'rgba(255,255,255,.06)',
                fontFamily: body,
                fontSize: 34,
                fontWeight: 600,
                color: BRAND.cream,
              }}
            >
              {m}
            </div>
          ))}
        </div>
        <div
          style={{
            display: 'inline-block',
            marginTop: 40,
            padding: '22px 54px',
            borderRadius: 100,
            background: `linear-gradient(120deg, ${BRAND.bright}, ${BRAND.deep})`,
            fontFamily: display,
            fontSize: 46,
            fontWeight: 700,
            color: '#fff',
            transform: `scale(${pillIn})`,
          }}
        >
          Бесплатно. Без карты
        </div>
        <div
          style={{
            marginTop: 36,
            fontFamily: display,
            fontSize: 38,
            fontWeight: 500,
            color: BRAND.bright,
            opacity: pillIn,
          }}
        >
          tryberry.ru
        </div>
        <div
          style={{
            marginTop: 36,
            fontFamily: body,
            fontSize: 36,
            color: 'rgba(255,247,251,.75)',
            opacity: pillIn,
            transform: `translateY(${arrowBob}px)`,
          }}
        >
          ссылка в описании ↓
        </div>
      </div>
    </AbsoluteFill>
  );
};
