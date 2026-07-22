import React from 'react';
import {AbsoluteFill, useCurrentFrame, useVideoConfig, spring, interpolate} from 'remotion';
import {BRAND, rub} from './brand';
import {display, body} from './fonts';

// Блок G реестра ассетов: полноэкранные графики-доказательства для C1-роликов.
// Титры-нарратив кладутся на монтаже; здесь только сам график и подписи ДАННЫХ
// (цены, опорные, ярлыки) — это содержание графика, а не титры.
// Оформление то же, что в ChatScene/сайте: ступени berry, опорные с подписями.
// Цифры — ⟨ПОДСТАВИТЬ⟩ из реальных серий БД перед чистовым рендером.

const C = {
  berry: '#e8336c',
  berryHi: '#ff5d8f',
  grid: 'rgba(232,51,108,.10)',
  good: '#3fe0a8',
  gold: '#ffc24a',
  fill: 'rgba(232,51,108,.10)',
};

type Marker = {i: number; label: string; at: number}; // точка серии + подпись-пилюля
type Ref = {v: number; color: string; label: string};

type StepChartProps = {
  series: number[];
  refs?: Ref[];
  // «Фейковая старая цена»: пунктир в пустоте НАД графиком + бейдж скидки.
  fake?: {v: number; label: string; badge: string; at: number};
  markers?: Marker[];
};

const StepChart: React.FC<StepChartProps> = ({series, refs = [], fake, markers = []}) => {
  const frame = useCurrentFrame();
  const {fps} = useVideoConfig();

  const draw = interpolate(frame, [8, 70], [0, 1], {extrapolateLeft: 'clamp', extrapolateRight: 'clamp'});
  const refsIn = interpolate(frame, [55, 80], [0, 1], {extrapolateLeft: 'clamp', extrapolateRight: 'clamp'});

  const W = 1000;
  const H = 860;
  const PAD = {l: 30, r: 30, t: 120, b: 60};
  // Шкала учитывает и фейковую цену (ей нужно место сверху)
  const lo = Math.min(...series, ...refs.map((r) => r.v)) * 0.96;
  const hi = Math.max(...series, ...(fake ? [fake.v] : []), ...refs.map((r) => r.v)) * 1.05;
  const x = (i: number) => PAD.l + (i / (series.length - 1)) * (W - PAD.l - PAD.r);
  const y = (v: number) => PAD.t + (1 - (v - lo) / (hi - lo)) * (H - PAD.t - PAD.b);

  let line = `M ${x(0)},${y(series[0])}`;
  for (let i = 1; i < series.length; i++) line += ` H ${x(i)} V ${y(series[i])}`;
  const area = `${line} V ${H - PAD.b} H ${x(0)} Z`;

  const fakeIn = fake
    ? spring({frame: frame - fake.at * fps, fps, config: {damping: 13, mass: 0.6}})
    : 0;

  return (
    <svg width="100%" viewBox={`0 0 ${W} ${H}`}>
      {[0.25, 0.5, 0.75].map((t) => (
        <line
          key={t}
          x1={PAD.l}
          y1={PAD.t + t * (H - PAD.t - PAD.b)}
          x2={W - PAD.r}
          y2={PAD.t + t * (H - PAD.t - PAD.b)}
          stroke={C.grid}
          strokeWidth={2}
        />
      ))}
      <path d={area} fill={C.fill} opacity={draw} />
      <path
        d={line}
        fill="none"
        stroke={C.berry}
        strokeWidth={6}
        strokeLinejoin="round"
        strokeLinecap="round"
        pathLength={1}
        strokeDasharray={1}
        strokeDashoffset={1 - draw}
      />
      {refs.map((r) => (
        <g key={r.label}>
          <line
            x1={PAD.l}
            y1={y(r.v)}
            x2={W - PAD.r}
            y2={y(r.v)}
            stroke={r.color}
            strokeWidth={3}
            strokeDasharray="14 12"
            opacity={refsIn * 0.75}
          />
          <text
            x={PAD.l + 6}
            y={y(r.v) - 12}
            fill={r.color}
            opacity={refsIn}
            style={{fontFamily: body, fontSize: 30, fontWeight: 600}}
          >
            {r.label}
          </text>
        </g>
      ))}
      {fake && (
        <g opacity={Math.min(fakeIn, 1)}>
          <line
            x1={PAD.l}
            y1={y(fake.v)}
            x2={W - PAD.r}
            y2={y(fake.v)}
            stroke={C.gold}
            strokeWidth={3.5}
            strokeDasharray="8 14"
          />
          <text
            x={PAD.l + 6}
            y={y(fake.v) - 14}
            fill={C.gold}
            style={{fontFamily: body, fontSize: 31, fontWeight: 600}}
          >
            {fake.label}
          </text>
          {/* бейдж «−60%» прыгает у фейковой линии */}
          <g transform={`translate(${W - PAD.r - 150}, ${y(fake.v) - 66}) scale(${Math.min(fakeIn, 1.12)})`}>
            <rect width={150} height={62} rx={31} fill={C.gold} />
            <text
              x={75}
              y={42}
              textAnchor="middle"
              fill="#1a0d14"
              style={{fontFamily: display, fontSize: 34, fontWeight: 700}}
            >
              {fake.badge}
            </text>
          </g>
        </g>
      )}
      {markers.map((mk) => {
        const s = spring({frame: frame - mk.at * fps, fps, config: {damping: 10, mass: 0.5}});
        const px = x(mk.i);
        const py = y(series[mk.i]);
        const left = mk.i > series.length / 2;
        return (
          <g key={mk.label} opacity={Math.min(s, 1)}>
            <circle cx={px} cy={py} r={13 * Math.min(s, 1.15)} fill={C.berryHi} stroke="#120a10" strokeWidth={5} />
            <g transform={`translate(${left ? px - 320 : px + 26}, ${py - 32})`}>
              <rect width={294} height={58} rx={29} fill="rgba(255,255,255,.08)" stroke="rgba(255,255,255,.16)" />
              <text
                x={147}
                y={39}
                textAnchor="middle"
                fill={BRAND.cream}
                style={{fontFamily: body, fontSize: 29, fontWeight: 600}}
              >
                {mk.label}
              </text>
            </g>
          </g>
        );
      })}
    </svg>
  );
};

// Обёртка кадра: тёмный фон со свечением, график в центре, снизу воздух под
// титры монтажа (сейф-зона).
const Frame: React.FC<{children: React.ReactNode}> = ({children}) => (
  <AbsoluteFill style={{background: '#120a10', justifyContent: 'center'}}>
    <AbsoluteFill
      style={{background: `radial-gradient(60% 40% at 80% 0%, ${BRAND.deep}55, transparent 70%)`}}
    />
    <div style={{padding: '0 40px'}}>{children}</div>
  </AbsoluteFill>
);

// ── G1. «Фейковая скидка»: линия ровная, «старая цена» висит в пустоте ──
export const FakeDiscount: React.FC = () => (
  <Frame>
    <StepChart
      series={[2490, 2490, 2590, 2490, 2440, 2490, 2590, 2490, 2490, 2540, 2490, 2490]}
      fake={{v: 6225, label: '«старая цена» 6 225 ₽ — здесь её не было', badge: '−60%', at: 3.0}}
      refs={[{v: 2490, color: C.good, label: 'реальная цена ~2 490 ₽'}]}
    />
  </Frame>
);

// ── G2. «Провал цены»: стабильно → падение, маркер алерта ──
export const DropAlert: React.FC = () => (
  <Frame>
    <StepChart
      series={[17990, 17990, 17490, 18490, 16990, 17490, 17990, 17490, 18990, 17490, 13216, 13216]}
      refs={[{v: 17500, color: C.gold, label: 'обычная ~17 500 ₽'}]}
      markers={[{i: 10, label: '🔔 алерт · −4 774 ₽', at: 3.2}]}
    />
  </Frame>
);

// ── G3. «Качели»: цена гуляет ±30% — покупать надо на дне ──
export const Rollercoaster: React.FC = () => (
  <Frame>
    <StepChart
      series={[2990, 2490, 3490, 2790, 2290, 3290, 2590, 3090, 2390, 2890, 3490, 2190]}
      markers={[
        {i: 10, label: 'пик · 3 490 ₽', at: 3.0},
        {i: 11, label: 'дно · 2 190 ₽', at: 3.8},
      ]}
    />
  </Frame>
);
