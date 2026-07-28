// Генератор PNG кадра стыка — того самого, который отдаётся Kling конечным
// кадром (канон: docs/content/hooks.md, «Конечный кадр — наш PNG»).
//
// Зачем отдельный путь, если есть композиция SeamFrame: в WSL этой машины
// `remotion still` не работает — mirrored-networking ломает loopback и Chrome
// DevTools не коннектится. Кадр при этом плоский (заливка + один радиальный
// градиент), браузер для него не нужен. Числа берём из brand.ts, те же, что
// уходят в CSS: разъехаться не могут по построению.
//
//   node scripts/seam-frame.mjs [выходной.png]
//
// Дизеринг обязателен: на почти чёрном фоне градиент занимает единицы уровней
// яркости, и без него по свечению идут кольца. Kling получает кадр как цель и
// такие кольца охотно запекает в последние кадры генерации. Матрица Байера —
// упорядоченная, значит результат воспроизводим (в отличие от шума).
// Эталон, если понадобится сверка: `npx remotion still SeamFrame ...` с Windows.

import {deflateSync} from 'node:zlib';
import {writeFileSync} from 'node:fs';
import {BRAND, SEAM_GLOW} from '../src/brand.ts';

const W = 1080;
const H = 1920;

const hex = (s) => [1, 3, 5].map((i) => parseInt(s.slice(i, i + 2), 16));

const bg = hex(BRAND.bg);
const glow = hex(BRAND.deep);
const {rx, ry, cx, cy, alpha, stop} = SEAM_GLOW;

// CSS radial-gradient(rx ry at cx cy, color, transparent stop):
// t — расстояние до центра в долях радиуса эллипса, на t=stop альфа = 0.
// Браузер интерполирует в premultiplied alpha, поэтому цвет постоянен, а к нулю
// едет только альфа — без этого на стыке с `transparent` вылезала бы чернота.
// Матрица Байера 8×8 → смещение в [-0.5, 0.5) перед округлением.
const BAYER = [
  [0, 32, 8, 40, 2, 34, 10, 42],
  [48, 16, 56, 24, 50, 18, 58, 26],
  [12, 44, 4, 36, 14, 46, 6, 38],
  [60, 28, 52, 20, 62, 30, 54, 22],
  [3, 35, 11, 43, 1, 33, 9, 41],
  [51, 19, 59, 27, 49, 17, 57, 25],
  [15, 47, 7, 39, 13, 45, 5, 37],
  [63, 31, 55, 23, 61, 29, 53, 21],
];

const raw = Buffer.alloc(H * (1 + W * 3));
for (let y = 0; y < H; y++) {
  const rowStart = y * (1 + W * 3);
  raw[rowStart] = 0; // PNG filter type 0 (None)
  const dy = (y - cy * H) / (ry * H);
  const bayerRow = BAYER[y & 7];
  for (let x = 0; x < W; x++) {
    const dx = (x - cx * W) / (rx * W);
    const t = Math.hypot(dx, dy);
    const a = t >= stop ? 0 : alpha * (1 - t / stop);
    const d = bayerRow[x & 7] / 64 - 0.5;
    const p = rowStart + 1 + x * 3;
    for (let c = 0; c < 3; c++) {
      const v = bg[c] * (1 - a) + glow[c] * a + d;
      raw[p + c] = Math.max(0, Math.min(255, Math.round(v)));
    }
  }
}

const chunk = (type, data) => {
  const len = Buffer.alloc(4);
  len.writeUInt32BE(data.length);
  const body = Buffer.concat([Buffer.from(type, 'ascii'), data]);
  const crc = Buffer.alloc(4);
  crc.writeUInt32BE(crc32(body) >>> 0);
  return Buffer.concat([len, body, crc]);
};

const CRC_TABLE = Array.from({length: 256}, (_, n) => {
  let c = n;
  for (let k = 0; k < 8; k++) c = c & 1 ? 0xedb88320 ^ (c >>> 1) : c >>> 1;
  return c >>> 0;
});
function crc32(buf) {
  let c = 0xffffffff;
  for (const b of buf) c = CRC_TABLE[(c ^ b) & 0xff] ^ (c >>> 8);
  return c ^ 0xffffffff;
}

const ihdr = Buffer.alloc(13);
ihdr.writeUInt32BE(W, 0);
ihdr.writeUInt32BE(H, 4);
ihdr[8] = 8; // бит на канал
ihdr[9] = 2; // truecolor RGB
const png = Buffer.concat([
  Buffer.from([0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a]),
  chunk('IHDR', ihdr),
  chunk('IDAT', deflateSync(raw, {level: 9})),
  chunk('IEND', Buffer.alloc(0)),
]);

const out = process.argv[2] ?? 'out/seam-frame.png';
writeFileSync(out, png);
console.log(`${out} — ${W}×${H}, ${(png.length / 1024).toFixed(0)} КБ`);
