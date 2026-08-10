import React from 'react';
import {
  AbsoluteFill,
  Img,
  interpolate,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
} from 'remotion';
import {BRAND, SEAM_GLOW} from './brand';

// Хук — первые 3 секунды ролика, под первую фразу озвучки.
//
// В кадре НЕТ текста: название площадки произносит озвучка и дублируют
// субтитры, которые кладутся поверх на монтаже. Дублировать его ещё и титром
// внутри композиции незачем — три надписи об одном и том же.
//
// Почему это рендер, а не футаж (решение владельца 2026-08-09): на трёх
// секундах ни сток, ни генерация не успевают ничего сказать — зритель их
// только опознаёт, и кадр кончается. Ровно на этом осыпался хук GEN-01
// (обрыв 77% → 40% между 1-й и 2-й секундой, разбор retro-gen-01.md).
//
// Нижняя треть кадра оставлена пустой: туда лягут субтитры и туда же смотрит
// сейф-зона под интерфейс площадок.
//
// Кадр заканчивается тем же свечением, что и подложка чата (SEAM_GLOW), —
// склейка хук→сцена получается незаметной, ради этого же в композициях чата
// живёт LEAD_IN.

// ⚠️ Логотипы площадок в репозитории НЕ лежат — это чужие товарные знаки, и
// класть их файлы в git не стоит. Чтобы включить логотип в фоне:
//   1. положить PNG с прозрачным фоном в promo/remotion/public/brands/
//      под именами wb.png, ozon.png, ym.png (mp — без логотипа, там их четыре);
//   2. переключить флаг ниже в true.
// Пока флаг false, хук рендерится как чистое цветовое поле — оно самодостаточно
// и субтитры на нём читаются.
const LOGOS_READY = false;

export type HookTint = {label: string; from: string; to: string; logo?: string};

export const HOOK_TINTS: Record<string, HookTint> = {
  wb: {label: 'Wildberries', from: '#cb11ab', to: '#5b1a8c', logo: 'brands/wb.png'},
  ozon: {label: 'Ozon', from: '#005bff', to: '#0a2472', logo: 'brands/ozon.png'},
  ym: {label: 'Яндекс Маркет', from: '#ffcc00', to: '#b37400', logo: 'brands/ym.png'},
  mp: {label: 'Маркетплейсы', from: BRAND.bright, to: BRAND.deep},
};

export const Hook: React.FC<{tint: HookTint}> = ({tint}) => {
  const frame = useCurrentFrame();
  const {durationInFrames} = useVideoConfig();
  const p = frame / durationInFrames;

  // Медленный наезд на весь кадр — движение есть с нулевого кадра, но глаз за
  // ним не гонится и не мешает читать субтитры.
  const push = interpolate(p, [0, 1], [1, 1.08]);

  // Два пятна света расходятся: верхнее в цвете площадки, нижнее уводит в наш
  // берри, чтобы к концу хука подложка совпала с первым кадром чат-сцены.
  const glowY = interpolate(p, [0, 1], [26, 16]);
  const glowX = interpolate(p, [0, 1], [50, 62]);
  const handoff = interpolate(p, [0.55, 1], [0, 1], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });

  const seam = `radial-gradient(${SEAM_GLOW.rx * 100}% ${SEAM_GLOW.ry * 100}% at ${
    SEAM_GLOW.cx * 100
  }% ${SEAM_GLOW.cy * 100}%, ${BRAND.deep}${Math.round(SEAM_GLOW.alpha * 255 * handoff)
    .toString(16)
    .padStart(2, '0')}, transparent ${SEAM_GLOW.stop * 100}%)`;

  const logoIn = interpolate(frame, [0, 14], [0, 1], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });

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

        {/* Логотип живёт в фоне: приглушённый и крупный, он не спорит с
            субтитрами, но площадка узнаётся с первого кадра. */}
        {LOGOS_READY && tint.logo && (
          <AbsoluteFill style={{alignItems: 'center', justifyContent: 'flex-start', paddingTop: 560}}>
            <Img
              src={staticFile(tint.logo)}
              style={{
                width: 620,
                opacity: 0.22 * logoIn,
                filter: 'saturate(0.2) brightness(1.6)',
                transform: `scale(${interpolate(p, [0, 1], [1, 1.05])})`,
              }}
            />
          </AbsoluteFill>
        )}

        {/* передача подложки чат-сцене */}
        <AbsoluteFill style={{background: seam}} />
      </AbsoluteFill>
    </AbsoluteFill>
  );
};
