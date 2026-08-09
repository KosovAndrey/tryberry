import React from 'react';
import {Composition, Folder} from 'remotion';
import {ChatSceneLeadIn, SeamBackdrop} from './ChatScene';
import {Endcard} from './Endcard';
import {
  alertIphone,
  alertBuds,
  searchIphone,
  searchBuds,
  searchBudsCut,
  alertIphoneTail,
  searchIphoneLong,
  searchIphoneLongWb,
  searchIphoneLongYm,
} from './data';

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
      {/* ── Что рендерить сейчас ────────────────────────────────────────────
          Продуктовая сцена мастера GEN-01 (0:26–0:46) в трёх версиях по
          площадкам — docs/content/gen01-marketplace-versions.md. Рендерятся все
          три сразу: npm run render:gen01 → out/gen01/. Длительность у них одна,
          озвучка мастера записана, и сдвиг разъедет монтаж. */}
      {/* ⚠️ Имя папки — только [a-zA-Z0-9-]: кириллица валит РЕНДЕР, а не сборку
          (Remotion validateFolderName), поэтому tsc и bundle её пропускают.
          Цифра в начале задаёт порядок в Studio. */}
      <Folder name="1-GEN01-render">
        {chat('GEN01-OZON', searchIphoneLong, 18.37)}
        {chat('GEN01-WB', searchIphoneLongWb, 18.37)}
        {chat('GEN01-YM', searchIphoneLongYm, 18.37)}
      </Folder>

      {/* ── Общее для всех роликов ──────────────────────────────────────── */}
      <Folder name="2-common">
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
      </Folder>

      {/* ── Архив: в работе НЕ участвует ──────────────────────────────────
          Отдельные ролики батча-02 (cold open + сжатый флоу) и две сцены,
          которые заменила GEN01-OZON: два чата подряд читались как повтор.
          Держим ради истории и на случай новых батчей — не удаляем, но и не
          рендерим. */}
      <Folder name="3-archive">
        {chat('ChatAlertIphone', alertIphone, 11.8)}
        {chat('ChatAlertBuds', alertBuds, 11.8)}
        {chat('ChatSearchIphone', searchIphone, 7.5)}
        {chat('ChatSearchBuds', searchBuds, 7.5)}
        {chat('ChatSearchBudsCut', searchBudsCut, 8.4)}
        {chat('ChatAlertIphoneTail', alertIphoneTail, 5.9)}
      </Folder>
    </>
  );
};
