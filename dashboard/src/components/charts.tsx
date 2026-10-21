import { useEffect, useRef, useState, type KeyboardEvent, type ReactNode } from "react";
import type { SpanPayload } from "../lib/api";
import { duration, opLabels } from "../lib/perf";

// Charts for the performance pages. Hand-written SVG: two small chart types
// don't justify a charting library, and this way every color is a token.
//
// Rules they follow (see the dataviz notes in docs): one y-axis; thin 2px
// lines; solid hairline grid; text in text colors, never the series color;
// a tooltip that follows a crosshair and also works from the keyboard; and
// the same numbers in a table, so nothing is readable only by hovering.

export interface LineSeries {
  key: string;
  label: string;
  values: (number | null)[];
  /** "accent": the series the chart is about. "context": gray, for comparison. */
  tone: "accent" | "context";
}

const H = 180;
const PAD = { top: 12, right: 44, bottom: 24, left: 52 };

/** Width of the element, following resizes (a phone rotating, the sidebar opening). */
function useWidth<T extends HTMLElement>(fallback = 640) {
  const ref = useRef<T>(null);
  const [width, setWidth] = useState(fallback);
  useEffect(() => {
    const el = ref.current;
    if (!el) return;
    setWidth(el.clientWidth || fallback);
    if (typeof ResizeObserver === "undefined") return;
    const ro = new ResizeObserver(() => setWidth(el.clientWidth || fallback));
    ro.observe(el);
    return () => ro.disconnect();
  }, [fallback]);
  return [ref, width] as const;
}

/** Round, readable tick values from 0 up past max: 0, 250, 500, 750. */
export function niceTicks(max: number, count = 4): number[] {
  if (!(max > 0)) return [0, 1];
  const raw = max / count;
  const mag = 10 ** Math.floor(Math.log10(raw));
  const step = [1, 2, 2.5, 5, 10].map((m) => m * mag).find((s) => s >= raw) ?? raw;
  const ticks: number[] = [];
  for (let v = 0; v <= max + step * 0.001; v += step) ticks.push(v);
  if (ticks[ticks.length - 1]! < max) ticks.push(ticks[ticks.length - 1]! + step);
  return ticks;
}

export function LineChart({
  title,
  labels,
  series,
  format,
  axisFormat = format,
  yMin,
  extraRows,
}: {
  title: string;
  labels: string[];
  series: LineSeries[];
  /** Values in the tooltip and table. */
  format: (v: number) => string;
  /** Tick labels, which are round numbers: "95%", not "95.0%". Defaults to format. */
  axisFormat?: (v: number) => string;
  /** Start the y-axis here instead of 0 (a crash-free rate lives between 90 and 100 %). */
  yMin?: number;
  /** More lines for the tooltip and the table at index i, e.g. how many sessions a point is based on. */
  extraRows?: { label: string; value: (i: number) => string };
}) {
  const [wrap, width] = useWidth<HTMLDivElement>();
  const [active, setActive] = useState<number | null>(null);
  const n = labels.length;
  const all = series.flatMap((s) => s.values.filter((v): v is number => v !== null));
  const lo = yMin ?? 0;
  const ticks = yMin !== undefined ? rangeTicks(lo, Math.max(lo + 1e-9, ...all, lo)) : niceTicks(Math.max(0, ...all));
  const hi = ticks[ticks.length - 1]!;
  const plotW = Math.max(10, width - PAD.left - PAD.right);
  const plotH = H - PAD.top - PAD.bottom;
  const x = (i: number) => PAD.left + (n <= 1 ? plotW / 2 : (i / (n - 1)) * plotW);
  const y = (v: number) => PAD.top + plotH - ((v - ticks[0]!) / (hi - ticks[0]! || 1)) * plotH;

  // A line breaks where there's no data, instead of inventing a straight
  // line across an hour that had no sessions.
  const path = (values: (number | null)[]) => {
    let d = "";
    let pen = false;
    values.forEach((v, i) => {
      if (v === null) return void (pen = false);
      d += `${pen ? "L" : "M"}${x(i).toFixed(1)},${y(v).toFixed(1)}`;
      pen = true;
    });
    return d;
  };
  const isolated = (values: (number | null)[], i: number) => values[i] !== null && values[i - 1] == null && values[i + 1] == null;

  function onPointer(e: React.PointerEvent<SVGRectElement>) {
    const box = e.currentTarget.getBoundingClientRect();
    const rel = (e.clientX - box.left) / (box.width || 1);
    setActive(Math.max(0, Math.min(n - 1, Math.round(rel * (n - 1)))));
  }
  function onKey(e: KeyboardEvent) {
    if (e.key === "ArrowRight" || e.key === "ArrowLeft") {
      e.preventDefault();
      const d = e.key === "ArrowRight" ? 1 : -1;
      setActive((a) => Math.max(0, Math.min(n - 1, (a ?? (d > 0 ? -1 : n)) + d)));
    } else if (e.key === "Escape") setActive(null);
  }

  const tone = (s: LineSeries) => (s.tone === "accent" ? "var(--brand)" : "var(--text-faint)");
  const showLabels = series.length > 1; // one series: the title already names it
  const tipLeft = active === null ? 0 : Math.min(Math.max(x(active) + 12, 0), width - 190);

  return (
    <figure className="line-chart" aria-label={title}>
      {showLabels && (
        <figcaption className="legend">
          {series.map((s) => (
            <span key={s.key}>
              <svg width="14" height="8" aria-hidden="true">
                <line x1="0" y1="4" x2="14" y2="4" stroke={tone(s)} strokeWidth="2" strokeLinecap="round" />
              </svg>
              {s.label}
            </span>
          ))}
        </figcaption>
      )}
      <div className="line-chart-plot" ref={wrap}>
        <svg
          width={width}
          height={H}
          role="img"
          aria-label={`${title}. Use the left and right arrow keys to read each point.`}
          tabIndex={0}
          onKeyDown={onKey}
          onBlur={() => setActive(null)}
        >
          {ticks.map((t) => (
            <g key={t}>
              <line x1={PAD.left} x2={PAD.left + plotW} y1={y(t)} y2={y(t)} stroke="var(--line)" strokeWidth="1" />
              <text x={PAD.left - 8} y={y(t) + 4} textAnchor="end" className="axis">
                {axisFormat(t)}
              </text>
            </g>
          ))}
          {labels.map((l, i) =>
            // About six labels across, whatever the width, so they never overlap.
            i % Math.max(1, Math.ceil(n / Math.max(2, Math.floor(plotW / 90)))) === 0 ? (
              <text key={i} x={x(i)} y={H - 6} textAnchor="middle" className="axis">
                {l}
              </text>
            ) : null,
          )}
          {active !== null && <line x1={x(active)} x2={x(active)} y1={PAD.top} y2={PAD.top + plotH} stroke="var(--line-strong)" strokeWidth="1" />}
          {series.map((s) => (
            <g key={s.key}>
              <path d={path(s.values)} fill="none" stroke={tone(s)} strokeWidth="2" strokeLinejoin="round" strokeLinecap="round" />
              {s.values.map((v, i) =>
                v !== null && (isolated(s.values, i) || i === active) ? (
                  <circle key={i} cx={x(i)} cy={y(v)} r="4" fill={tone(s)} stroke="var(--surface)" strokeWidth="2" />
                ) : null,
              )}
              {showLabels && <EndLabel series={s} x={x} y={y} />}
            </g>
          ))}
          <rect
            x={PAD.left}
            y={PAD.top}
            width={plotW}
            height={plotH}
            fill="transparent"
            onPointerMove={onPointer}
            onPointerLeave={() => setActive(null)}
          />
        </svg>
        {active !== null && (
          <div className="line-chart-tip" style={{ left: tipLeft }} role="status">
            <div className="faint">{labels[active]}</div>
            {series.map((s) => (
              <div key={s.key} className="tip-row">
                <svg width="10" height="8" aria-hidden="true">
                  <line x1="0" y1="4" x2="10" y2="4" stroke={tone(s)} strokeWidth="2" />
                </svg>
                <strong>{s.values[active] === null ? "no data" : format(s.values[active]!)}</strong>
                <span className="muted">{s.label}</span>
              </div>
            ))}
            {extraRows && (
              <div className="tip-row">
                <span style={{ width: 10 }} />
                <strong>{extraRows.value(active)}</strong>
                <span className="muted">{extraRows.label}</span>
              </div>
            )}
          </div>
        )}
      </div>
      <details className="line-chart-table">
        <summary>Show as a table</summary>
        <div style={{ overflowX: "auto" }}>
          <table className="table">
            <thead>
              <tr>
                <th>Time</th>
                {series.map((s) => (
                  <th key={s.key} className="num">
                    {s.label}
                  </th>
                ))}
                {extraRows && <th className="num">{extraRows.label}</th>}
              </tr>
            </thead>
            <tbody>
              {labels.map((l, i) => (
                <tr key={i}>
                  <td>{l}</td>
                  {series.map((s) => (
                    <td key={s.key} className="num mono">
                      {s.values[i] === null ? "–" : format(s.values[i]!)}
                    </td>
                  ))}
                  {extraRows && <td className="num mono">{extraRows.value(i)}</td>}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      </details>
    </figure>
  );
}

/** Ticks between lo and hi, for an axis that doesn't start at 0. */
function rangeTicks(lo: number, hi: number): number[] {
  const step = niceTicks(hi - lo, 3)[1]!;
  const out: number[] = [];
  for (let v = lo; v < hi + step * 0.999; v += step) out.push(Math.round(v * 1e6) / 1e6);
  return out;
}

function EndLabel({ series, x, y }: { series: LineSeries; x: (i: number) => number; y: (v: number) => number }) {
  let i = series.values.length - 1;
  while (i >= 0 && series.values[i] === null) i--;
  if (i < 0) return null;
  return (
    <text x={x(i) + 8} y={y(series.values[i]!) + 4} className="axis end-label">
      {series.key}
    </text>
  );
}

const spanStatus: Record<string, string> = { error: "Server error", failed: "Failed", cancelled: "Never finished", deadline_exceeded: "Timed out" };

/**
 * The spans of one transaction on its timeline: when each step started and
 * how long it took. Gaps between bars are time spent in the app's own code
 * (rendering, JavaScript), which no span covers.
 */
export function Waterfall({ spans, total }: { spans: SpanPayload[]; total: number }) {
  const sorted = [...spans].sort((a, b) => a.start_ms - b.start_ms);
  const scale = Math.max(total, ...sorted.map((s) => s.start_ms + s.duration_ms), 1);
  if (sorted.length === 0) return <p className="muted">No spans: nothing inside this transaction was timed separately.</p>;
  return (
    <div className="waterfall" role="table" aria-label="Spans">
      <div className="wf-row wf-head" role="row">
        <span role="columnheader">Step</span>
        <span role="columnheader">When, out of {duration(total)}</span>
      </div>
      {sorted.map((s) => {
        const bad = s.status && s.status !== "ok" ? s.status : undefined;
        const left = (s.start_ms / scale) * 100;
        const w = Math.max(0.4, (s.duration_ms / scale) * 100);
        return (
          <div className="wf-row" role="row" key={s.span_id + s.start_ms}>
            <span className="wf-label" role="cell">
              <span className="chip">{opLabels[s.op] ?? s.op}</span>
              <span className="wf-desc mono">{s.description ?? ""}</span>
              {bad && <span className="chip unresolved">{spanStatus[bad] ?? bad}</span>}
            </span>
            <span className="wf-track" role="cell" title={`starts at ${duration(s.start_ms)}, takes ${duration(s.duration_ms)}`}>
              <span className={`wf-bar${bad ? " bad" : ""}`} style={{ left: `${left}%`, width: `${w}%` }} />
              <span className="wf-time mono" style={left + w > 78 ? { right: `${100 - left + 1}%` } : { left: `calc(${left + w}% + 6px)` }}>
                {duration(s.duration_ms)}
              </span>
            </span>
          </div>
        );
      })}
    </div>
  );
}

export function Empty({ title, children }: { title: string; children: ReactNode }) {
  return (
    <div className="empty">
      <h2>{title}</h2>
      <div className="muted" style={{ maxWidth: 560 }}>
        {children}
      </div>
    </div>
  );
}
