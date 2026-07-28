import React from 'react';
import {Composition} from 'remotion';
import {ChatSceneLeadIn, SeamBackdrop} from './ChatScene';
import {Endcard} from './Endcard';
import {alertIphone, alertBuds, searchIphone, searchBuds} from './data';

// Лид-ин: кадры чистой подложки перед первым сообщением. Нужны под стык с
// AI-хуком (docs/content/hooks.md): последний кадр хука = первый кадр середины.
// 10 кадров = треть секунды — паузой не читается, но даёт монтажу запас на
// подгонку склейки и место, куда посадить вспышку, если стык пойдёт по fallback.
const LEAD_IN = 10;

// Длительность = лид-ин + последнее сообщение + хвост на прочтение.
// Хвост 1.3с: кому мало — перемотают/поставят паузу (фидбек 2026-07-23).
const dur = (lastAt: number, fps = 30, tail = 1.3) => Math.round((lastAt + tail) * fps);

const chat = (id: string, data: typeof alertIphone, lastAt: number) => (
  <Composition
    id={id}
    component={ChatSceneLeadIn}
    durationInFrames={LEAD_IN + dur(lastAt)}
    fps={30}
    width={1080}
    height={1920}
    defaultProps={{...data, leadIn: LEAD_IN}}
  />
);

export const RemotionRoot: React.FC = () => {
  return (
    <>
      {/* Один шаблон ChatScene × 4 набора данных: 2 товара × 2 сценария.
          ВСЕ в новом формате (вопрос-cold-open + сжатый флоу, ~8.5-9с).
          У каждого ролика свой паттерн вопроса — мини-тест формулировок. */}
      {chat('ChatAlertIphone', alertIphone, 11.8)}
      {chat('ChatAlertBuds', alertBuds, 11.8)}
      {chat('ChatSearchIphone', searchIphone, 7.5)}
      {chat('ChatSearchBuds', searchBuds, 7.5)}
      {/* B1 — эндкард, одинаковый во всех роликах (отличительный актив) */}
      <Composition id="Endcard" component={Endcard} durationInFrames={120} fps={30} width={1080} height={1920} />
      {/* Кадр стыка: экспортируется в PNG и отдаётся Kling конечным кадром.
          npx remotion still SeamFrame out/seam-frame.png */}
      <Composition
        id="SeamFrame"
        component={SeamBackdrop}
        durationInFrames={1}
        fps={30}
        width={1080}
        height={1920}
      />
    </>
  );
};
