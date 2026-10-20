// Events per hour over 24 hours. Bars, not a line: zero hours read as
// empty, which is the honest picture for bursty errors.
export function Sparkline({ values, width = 110, height = 26, label }: { values: number[]; width?: number; height?: number; label?: string }) {
  const max = Math.max(1, ...values);
  const bar = width / Math.max(1, values.length);
  const total = values.reduce((a, b) => a + b, 0);
  return (
    <svg width={width} height={height} viewBox={`0 0 ${width} ${height}`} role="img" aria-label={label ?? `${total} events in the last 24 hours`}>
      <title>{label ?? `${total} events in the last 24 hours`}</title>
      {values.map((v, i) => {
        const h = v === 0 ? 1 : Math.max(2, (v / max) * (height - 2));
        return <rect key={i} x={i * bar + 0.5} y={height - h} width={Math.max(1, bar - 1.5)} height={h} rx={1} fill={v === 0 ? "var(--line-strong)" : "var(--brand)"} />;
      })}
    </svg>
  );
}
