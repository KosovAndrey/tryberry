import React from 'react';
import {Composition} from 'remotion';
import {ChatScene} from './ChatScene';
import {Endcard} from './Endcard';
import {alertIphone, alertBuds, searchIphone, searchBuds} from './data';

// Длительность = последнее сообщение + хвост на прочтение.
const dur = (lastAt: number, fps = 30, tail = 3.5) => Math.round((lastAt + tail) * fps);

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
      {/* Один шаблон ChatScene × 4 набора данных: 2 товара × 2 сценария. */}
      {chat('ChatAlertIphone', alertIphone, 16.0)}
      {chat('ChatAlertBuds', alertBuds, 16.0)}
      {chat('ChatSearchIphone', searchIphone, 10.8)}
      {chat('ChatSearchBuds', searchBuds, 10.8)}
      {/* B1 — эндкард, одинаковый во всех роликах (отличительный актив) */}
      <Composition id="Endcard" component={Endcard} durationInFrames={120} fps={30} width={1080} height={1920} />
    </>
  );
};
