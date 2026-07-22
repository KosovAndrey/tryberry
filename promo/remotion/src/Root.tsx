import React from 'react';
import {Composition} from 'remotion';
import {ChatScene} from './ChatScene';
import {alertDemo, searchDemo} from './data';

// Длительность выводим из последнего сообщения + хвост на прочтение.
const durationFrames = (lastAt: number, fps = 30, tail = 2.5) =>
  Math.round((lastAt + tail) * fps);

export const RemotionRoot: React.FC = () => {
  return (
    <>
      {/* Один шаблон ChatScene, много композиций через props — вот реюз. */}
      <Composition
        id="ChatAlert"
        component={ChatScene}
        durationInFrames={durationFrames(3.6)}
        fps={30}
        width={1080}
        height={1920}
        defaultProps={alertDemo}
      />
      <Composition
        id="ChatSearch"
        component={ChatScene}
        durationInFrames={durationFrames(3.8)}
        fps={30}
        width={1080}
        height={1920}
        defaultProps={searchDemo}
      />
    </>
  );
};
