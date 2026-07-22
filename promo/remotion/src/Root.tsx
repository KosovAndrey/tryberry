import React from 'react';
import {Composition} from 'remotion';
import {ChatScene} from './ChatScene';
import {Endcard} from './Endcard';
import {alertDemo, searchDemo} from './data';

// Длительность = последнее сообщение + хвост на прочтение алерта.
const dur = (lastAt: number, fps = 30, tail = 3.5) => Math.round((lastAt + tail) * fps);

export const RemotionRoot: React.FC = () => {
  return (
    <>
      {/* Один шаблон ChatScene × разные данные — вот весь реюз. */}
      <Composition
        id="ChatAlert"
        component={ChatScene}
        durationInFrames={dur(15.8)}
        fps={30}
        width={1080}
        height={1920}
        defaultProps={alertDemo}
      />
      <Composition
        id="ChatSearch"
        component={ChatScene}
        durationInFrames={dur(10.8)}
        fps={30}
        width={1080}
        height={1920}
        defaultProps={searchDemo}
      />
      {/* B1 — эндкард, одинаковый во всех роликах (отличительный актив) */}
      <Composition id="Endcard" component={Endcard} durationInFrames={120} fps={30} width={1080} height={1920} />
    </>
  );
};
