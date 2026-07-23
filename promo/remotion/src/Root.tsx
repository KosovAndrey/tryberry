import React from 'react';
import {Composition} from 'remotion';
import {ChatScene} from './ChatScene';
import {Endcard} from './Endcard';
import {alertIphone, alertBuds, searchIphone, searchBuds} from './data';

// Длительность = последнее сообщение + хвост на прочтение.
// Хвост 1.3с: кому мало — перемотают/поставят паузу (фидбек 2026-07-23).
const dur = (lastAt: number, fps = 30, tail = 1.3) => Math.round((lastAt + tail) * fps);

const VOICES = ['dmitry', 'svetlana', 'andrew', 'ava', 'brian', 'emma', 'seraphina', 'florian', 'remy', 'vivienne'];

// Раскладка VO-фраз по битам ChatAlertIphone (абсолютное время, включая cold open)
const voFor = (v: string) => [
  {at: 0.15, src: `vo/alert-iphone/01-${v}.mp3`},
  {at: 3.35, src: `vo/alert-iphone/02-${v}.mp3`},
  {at: 4.2, src: `vo/alert-iphone/03-${v}.mp3`},
  {at: 7.5, src: `vo/alert-iphone/05-${v}.mp3`},
  {at: 10.4, src: `vo/alert-iphone/06-${v}.mp3`},
];

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
          ВСЕ в новом формате (вопрос-cold-open + сжатый флоу, ~8.5-9с).
          У каждого ролика свой паттерн вопроса — мини-тест формулировок. */}
      {chat('ChatAlertIphone', alertIphone, 11.8)}
      {chat('ChatAlertBuds', alertBuds, 11.8)}
      {chat('ChatSearchIphone', searchIphone, 7.5)}
      {chat('ChatSearchBuds', searchBuds, 7.5)}
      {/* Кастинг голосов: тот же ChatAlertIphone × 10 озвучек (5 фраз каждая).
          2 русских edge-tts + 8 мультиязычных (у части лёгкий акцент).
          Раскладка фраз по битам одна на всех. */}
      {VOICES.map((v) => (
        <Composition
          key={v}
          id={`ChatAlertIphone-${v}`}
          component={ChatScene}
          durationInFrames={dur(11.8)}
          fps={30}
          width={1080}
          height={1920}
          defaultProps={{...alertIphone, vo: voFor(v)}}
        />
      ))}
      {/* B1 — эндкард, одинаковый во всех роликах (отличительный актив) */}
      <Composition id="Endcard" component={Endcard} durationInFrames={120} fps={30} width={1080} height={1920} />
    </>
  );
};
