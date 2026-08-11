import React from 'react';
import {
  AbsoluteFill,
  Img,
  interpolate,
  staticFile,
  useCurrentFrame,
  useVideoConfig,
} from 'remotion';
import {BRAND} from './brand';

// Хук — первые 3 секунды ролика, под первую фразу озвучки.
//
// В кадре НЕТ текста: название площадки произносит озвучка и дублируют
// субтитры, которые кладутся поверх на монтаже.
//
// Почему это рендер, а не футаж (решение владельца 2026-08-09): на трёх
// секундах ни сток, ни генерация не успевают ничего сказать — зритель их
// только опознаёт, и кадр кончается. Ровно на этом осыпался хук GEN-01
// (обрыв 77% → 40% между 1-й и 2-й секундой, разбор retro-gen-01.md).
//
// ⚠️ ПЕРВАЯ ВЕРСИЯ ЗАБРАКОВАНА (2026-08-10). Цвет площадки лежал полупрозрачным
// пятном поверх почти чёрного фона — бренд не узнавался, а жёлтый Я.Маркета
// вообще уходил в грязно-оливковый. Правило: кадр должен БЫТЬ цвета площадки,
// а не намекать на него. Отсюда `base` (заливка) отдельно от `glow` (пятно
// света), и оба взяты в полную силу.

// ⚠️ Логотипы площадок в репозитории НЕ лежат — это чужие товарные знаки.
// Чтобы включить логотип в кадре: положить PNG с прозрачным фоном в
// public/brands/ (wb.png, ozon.png, ym.png) и переключить флаг в true.
// Инструкция — public/brands/README.md.
const LOGOS_READY = false;

export type HookTint = {
  label: string;
  base: string; // заливка кадра — основной цвет площадки
  glow: string; // пятно света сверху, светлее базы
  dark: boolean; // тёмный ли кадр: от этого зависит цвет логотипа
  logo?: string;
};

export const HOOK_TINTS: Record<string, HookTint> = {
  wb: {label: 'Wildberries', base: '#4a0d6b', glow: '#cb11ab', dark: true, logo: 'brands/wb.png'},
  ozon: {label: 'Ozon', base: '#00248c', glow: '#2b7bff', dark: true, logo: 'brands/ozon.png'},
  ym: {label: 'Яндекс Маркет', base: '#c99700', glow: '#ffd633', dark: false, logo: 'brands/ym.png'},
  mp: {label: 'Маркетплейсы', base: BRAND.deep, glow: BRAND.bright, dark: true},
};

// Зерно поверх заливки: на больших плавных градиентах Chrome кладёт кольца
// бандинга (та же грабля, что в scripts/seam-frame.mjs), и на видео их
// запекает кодек. Шум их разбивает.
const NOISE =
  "url(\"data:image/svg+xml;utf8,<svg xmlns='http://www.w3.org/2000/svg' width='240' height='240'>" +
  "<filter id='n'><feTurbulence type='fractalNoise' baseFrequency='0.9' numOctaves='3'/></filter>" +
  "<rect width='240' height='240' filter='url(%23n)'/></svg>\")";

export const Hook: React.FC<{tint: HookTint}> = ({tint}) => {
  const frame = useCurrentFrame();
  const {durationInFrames} = useVideoConfig();
  const p = frame / durationInFrames;

  // Медленный наезд — движение есть с нулевого кадра, но глаз за ним не
  // гонится и не мешает читать субтитры.
  const push = interpolate(p, [0, 1], [1, 1.07]);
  const glowX = interpolate(p, [0, 1], [50, 60]);
  const glowY = interpolate(p, [0, 1], [30, 22]);

  // К концу хука слегка притемняем — чтобы склейка с тёмной чат-сценой не
  // била по глазам. Не в ноль: резкая смена яркости на стыке работает как
  // смена кадра и удерживает внимание.
  const handoff = interpolate(p, [0.7, 1], [0, 0.35], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });

  const logoIn = interpolate(frame, [0, 14], [0, 1], {
    extrapolateLeft: 'clamp',
    extrapolateRight: 'clamp',
  });

  return (
    <AbsoluteFill style={{background: tint.base}}>
      <AbsoluteFill style={{transform: `scale(${push})`}}>
        {/* пятно света — объём, чтобы заливка не читалась плоской */}
        <AbsoluteFill
          style={{
            background: `radial-gradient(75% 50% at ${glowX}% ${glowY}%, ${tint.glow}, transparent 70%)`,
          }}
        />
        {/* виньетка по краям — взгляд собирается к центру */}
        <AbsoluteFill
          style={{background: 'radial-gradient(90% 60% at 50% 45%, transparent 40%, #00000073)'}}
        />
        {LOGOS_READY && tint.logo && (
          <AbsoluteFill style={{alignItems: 'center', justifyContent: 'center', paddingBottom: 380}}>
            <Img
              src={staticFile(tint.logo)}
              style={{
                width: 660,
                opacity: (tint.dark ? 0.9 : 0.85) * logoIn,
                transform: `scale(${interpolate(p, [0, 1], [1, 1.04])})`,
              }}
            />
          </AbsoluteFill>
        )}
      </AbsoluteFill>

      <AbsoluteFill style={{background: NOISE, opacity: 0.06, mixBlendMode: 'overlay'}} />

      {/* Тёмная подложка под нижней третью: субтитры кладутся на монтаже, и
          белый текст обязан читаться на любом фирменном цвете — на жёлтом
          Я.Маркета без неё он пропадает. */}
      <AbsoluteFill
        style={{background: 'linear-gradient(to bottom, transparent 52%, #000000d9 100%)'}}
      />

      <AbsoluteFill style={{background: BRAND.bg, opacity: handoff}} />
    </AbsoluteFill>
  );
};
