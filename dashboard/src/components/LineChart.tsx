import { useEffect, useRef, useState } from "react";
import { bucketLabel, niceTicks, type Interval } from "../lib/analytics";
import { compact } from "../lib/time";

export interface ChartSeries {
  label: string;
  values: number[];
}

// One line per series over shared time buckets, drawn as SVG at the
// container's real width (so axis text stays readable on a phone). Hover,
// or focus the chart and press ←/→, to read every series at one point.
export function LineChart({ buckets, series, interval, height = 260 }: { buckets: string[]; series: ChartSeries[]; interval: Interval; height?: number }) {
  const box = useRef<HTMLDivElement>(null);
  const [width, setWidth] = useState(640);
  const [hover, setHover] = useState<number | null>(null);

  useEffect(() => {
    const el = box.current;
    if (!el) return;
    const ro = new ResizeObserver(() => setWidth(Math.max(240, el.clientWidth)));
    ro.observe(el);
    setWidth(Math.max(240, el.clientWidth));
    return () => ro.disconnect();
  }, []);

  const pad = { top: 12, right: 12, bottom: 28, left: 44 };
  const w = width - pad.left - pad.right;
  const h = height - pad.top - pad.bottom;
  const max = Math.max(0, ...series.flatMap((s) => s.values));
  const ticks = niceTicks(max);
  const top = ticks[ticks.length - 1] || 1;
  const n = buckets.length;
  const x = (i: number) => pad.left + (n <= 1 ? w / 2 : (i / (n - 1)) * w);
  const y = (v: number) => pad.top + h - (v / top) * h;
  // About one x label per 90px, always including the last point.
  const every = Math.max(1, Math.ceil(n / Math.max(2, Math.floor(w / 90))));

  function pick(clientX: number) {
    const rect = box.current?.getBoundingClientRect();
    if (!rect || n === 0) return;
    const i = Math.round(((clientX - rect.left - pad.left) / w) * (n - 1));
    setHover(Math.min(n - 1, Math.max(0, i)));
  }

  const total = series.reduce((sum, s) => sum + s.values.reduce((a, b) => a + b, 0), 0);
  const label = `Line chart of ${series.length} series over ${n} ${interval}s, ${compact(total)} in total. The table below lists every value.`;

  return (
    <div
      ref={box}
      className="chart"
      tabIndex={0}
      role="img"
      aria-label={label}
      onMouseMove={(e) => pick(e.clientX)}
      onMouseLeave={() => setHover(null)}
      onBlur={() => setHover(null)}
      onKeyDown={(e) => {
        if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
          e.preventDefault();
          e.stopPropagation(); // not the page's shortcuts
          setHover((hv) => Math.min(n - 1, Math.max(0, (hv ?? (e.key === "ArrowRight" ? -1 : n)) + (e.key === "ArrowRight" ? 1 : -1))));
        }
      }}
    >
      <svg width={width} height={height} aria-hidden="true">
        {ticks.map((t) => (
          <g key={t}>
            <line x1={pad.left} x2={pad.left + w} y1={y(t)} y2={y(t)} className="chart-grid" />
            <text x={pad.left - 8} y={y(t)} className="chart-axis" textAnchor="end" dominantBaseline="middle">
              {compact(t)}
            </text>
          </g>
        ))}
        {buckets.map((b, i) =>
          // Every `every`-th label, plus the last; one that would sit within
          // 56px of the last label is skipped so they never overlap.
          i === n - 1 || (i % every === 0 && x(n - 1) - x(i) >= 56) ? (
            <text key={b} x={x(i)} y={height - 8} className="chart-axis" textAnchor={i === 0 ? "start" : i === n - 1 ? "end" : "middle"}>
              {bucketLabel(b, interval)}
            </text>
          ) : null,
        )}
        {series.length === 1 && n > 1 && (
          // One series: a soft area under the line reads as "how much".
          <path className="chart-area" style={{ fill: "var(--chart-1)" }} d={`M${x(0)},${y(0)} ${series[0]!.values.map((v, i) => `L${x(i)},${y(v)}`).join(" ")} L${x(n - 1)},${y(0)} Z`} />
        )}
        {series.map((s, si) => (
          <g key={s.label + si} style={{ color: `var(--chart-${(si % 10) + 1})` }}>
            <polyline className="chart-line" points={s.values.map((v, i) => `${x(i)},${y(v)}`).join(" ")} />
            {n > 0 && <circle className="chart-dot" cx={x(n - 1)} cy={y(s.values[n - 1] ?? 0)} r={3} />}
            {hover !== null && <circle className="chart-dot" cx={x(hover)} cy={y(s.values[hover] ?? 0)} r={4} />}
          </g>
        ))}
        {hover !== null && <line x1={x(hover)} x2={x(hover)} y1={pad.top} y2={pad.top + h} className="chart-guide" />}
      </svg>
      {hover !== null && (
        <div className="chart-tip" style={{ left: Math.min(Math.max(x(hover), 90), width - 90) }}>
          <div className="faint">{bucketLabel(buckets[hover]!, interval)}{interval === "hour" ? " UTC" : ""}</div>
          {series.slice(0, 6).map((s, si) => (
            <div key={s.label + si} className="chart-tip-row">
              <span className="swatch" style={{ background: `var(--chart-${(si % 10) + 1})` }} />
              <span className="chart-tip-label">{s.label}</span>
              <strong>{(s.values[hover] ?? 0).toLocaleString()}</strong>
            </div>
          ))}
          {series.length > 6 && <div className="faint">+{series.length - 6} more in the table</div>}
        </div>
      )}
    </div>
  );
}
