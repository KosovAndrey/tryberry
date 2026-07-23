import React from 'react';
import {Composition} from 'remotion';
import {ChatScene} from './ChatScene';
import {Endcard} from './Endcard';
import {alertIphone, alertBuds, searchIphone, searchBuds} from './data';

// Длительность = последнее сообщение + хвост на прочтение.
// Хвост 1.3с: кому мало — перемотают/поставят паузу (фидбек 2026-07-23).
const dur = (lastAt: number, fps = 30, tail = 1.3) => Math.round((lastAt + tail) * fps);

const chat = (id: string, data: typeof alertIphone, lastAt: number) => (
  <Composition
    id={id}
    component={ChatScene}
    durationInFrames={dur(lastAt)}
    fps={30}
    width={1080}
    height={1920}
    defaultProps={data}
  />
);

export const RemotionRoot: React.FC = () => {
  return (
    <>
      {/* Один шаблон ChatScene × 4 набора данных: 2 товара × 2 сценария.
          iPhone = НОВЫЙ формат (вопрос-cold-open + сжатие, ~10.6с; зум забракован),
          наушники = старый (полный флоу, ~20с) — сравниваем на реальных показах. */}
      {chat('ChatAlertIphone', alertIphone, 7.1)}
      {chat('ChatAlertBuds', alertBuds, 17.5)}
      {chat('ChatSearchIphone', searchIphone, 7.5)}
      {chat('ChatSearchBuds', searchBuds, 10.8)}
      {/* B1 — эндкард, одинаковый во всех роликах (отличительный актив) */}
      <Composition id="Endcard" component={Endcard} durationInFrames={120} fps={30} width={1080} height={1920} />
    </>
  );
};
