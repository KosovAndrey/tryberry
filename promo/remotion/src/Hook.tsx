import React from 'react';
import {AbsoluteFill, interpolate, spring, useCurrentFrame, useVideoConfig} from 'remotion';
import {BRAND, SEAM_GLOW} from './brand';
import {display, body} from './fonts';

// Хук — первые 3 секунды ролика, под первую фразу озвучки. Субтитры кладутся
// поверх на монтаже, поэтому нижняя треть кадра оставлена пустой.
//
// Почему это рендер, а не футаж (решение владельца 2026-08-09): на трёх
// секундах ни сток, ни генерация не успевают ничего сказать — зритель их
// только опознаёт, и кадр кончается. Ровно на этом осыпался хук GEN-01
// (обрыв 77% → 40% между 1-й и 2-й секундой, разбор retro-gen-01.md).
//
// Площадка обозначена ЦВЕТОМ и НАЗВАНИЕМ, а не логотипом: чужой товарный знак
// в своей рекламе — лишний риск, а файлов логотипов у нас всё равно нет.
// Цвета взяты как узнаваемые фирменные, но рисуем их своей типографикой.
//
// Кадр заканчивается тем же свечением, что и подложка чата (SEAM_GLOW), —
// склейка хук→сцена получается незаметной, ради этого же в композициях чата
// живёт LEAD_IN.

export type HookTint = {label: string; from: string; to: string};

export const HOOK_TINTS: Record<string, HookTint> = {
  wb: {label: 'Wildberries', from: '#cb11ab', to: '#5b1a8c'},
  ozon: {label: 'Ozon', from: '#005bff', to: '#0a2472'},
  ym: {label: 'Яндекс Маркет', from: '#ffcc00', to: '#b37400'},
  mp: {label: 'Маркетплейсы', from: BRAND.bright, to: BRAND.deep},
};

const SUB: Record<string, string | undefined> = {
  mp: 'Wildberries · Ozon · Я.Маркет · AliExpress',
};

export const Hook: React.FC<{tint: HookTint; sub?: string}> = ({tint, sub}) => {
  const frame = useCurrentFrame();
  const {fps, durationInFrames} = useVideoConfig();
  const p = frame / durationInFrames;

  // Медленный наезд на весь кадр — движение есть с нулевого кадра, но глаз
  // за ним не гонится и успевает прочитать титр.
  const push = interpolate(p, [0, 1], [1, 1.08]);

  // Два пятна света расходятся: верхнее в цвете площадки, нижнее уводит в наш
  // берри, чтобы к концу хука подложка совпала с первым кадром чат-сцены.
  const glowY = interpolate(p, [0, 1], [26, 16]);
  const glowX = interpolate(p, [0, 1], [50, 62]);
  const handoff = interpolate(p, [0.55, 1], [0, 1], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });

  const nameIn = spring({frame, fps, config: {damping: 14, mass: 0.7}});
  const subIn = spring({frame: frame - 10, fps, config: {damping: 14}});

  const seam = `radial-gradient(${SEAM_GLOW.rx * 100}% ${SEAM_GLOW.ry * 100}% at ${
    SEAM_GLOW.cx * 100
  }% ${SEAM_GLOW.cy * 100}%, ${BRAND.deep}${Math.round(SEAM_GLOW.alpha * 255 * handoff)
    .toString(16)
    .padStart(2, '0')}, transparent ${SEAM_GLOW.stop * 100}%)`;

  return (
    <AbsoluteFill style={{background: BRAND.bg}}>
      <AbsoluteFill style={{transform: `scale(${push})`}}>
        <AbsoluteFill
          style={{
            background: `radial-gradient(70% 45% at ${glowX}% ${glowY}%, ${tint.from}66, transparent 72%)`,
          }}
        />
        <AbsoluteFill
          style={{
            background: `radial-gradient(60% 40% at 30% 78%, ${tint.to}55, transparent 70%)`,
          }}
        />
        {/* передача подложки чат-сцене */}
        <AbsoluteFill style={{background: seam}} />
      </AbsoluteFill>

      {/* Титр сидит выше центра: нижняя треть отдана субтитрам, которые
          кладутся на монтаже, и сейф-зоне под UI площадок. */}
      <AbsoluteFill style={{alignItems: 'center', justifyContent: 'flex-start', paddingTop: 620}}>
        <div
          style={{
            fontFamily: display,
            fontWeight: 700,
            fontSize: sub ? 96 : 112,
            lineHeight: 1.05,
            letterSpacing: -2,
            textAlign: 'center',
            color: BRAND.cream,
            padding: '0 60px',
            opacity: nameIn,
            transform: `translateY(${interpolate(nameIn, [0, 1], [34, 0])}px)`,
            textShadow: `0 12px 60px ${tint.from}55`,
          }}
        >
          {tint.label}
        </div>
        {sub && (
          <div
            style={{
              fontFamily: body,
              fontWeight: 500,
              fontSize: 40,
              textAlign: 'center',
              color: BRAND.rose,
              marginTop: 28,
              padding: '0 60px',
              opacity: subIn * 0.9,
              transform: `translateY(${interpolate(subIn, [0, 1], [20, 0])}px)`,
            }}
          >
            {sub}
          </div>
        )}
      </AbsoluteFill>
    </AbsoluteFill>
  );
};

export const hookSub = (key: string) => SUB[key];
