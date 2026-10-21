import { useEffect, useState } from "react";
import { LineChart, Waterfall } from "../components/charts";
import type { Period, TransactionDetail, TransactionPayload } from "../lib/api";
import { useShortcuts } from "../lib/keys";
import { Link, navigate, useLocation } from "../lib/router";
import { bucketLabel, duration, durationTick, isPeriod, opLabels } from "../lib/perf";
import { compact, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";
import { PeriodFilter } from "./Performance";

const measurementLabels: Record<string, string> = {
  ttfb: "First byte from the server",
  fcp: "First content on screen",
  lcp: "Main content on screen",
  dom_content_loaded: "Page built (DOMContentLoaded)",
  load: "Everything loaded (load event)",
};

// One transaction name: its trend, where its time goes, how releases
// compare, and individual samples down to each span.
export function Transaction({ projectId }: { projectId: number }) {
  const { query } = useLocation();
  const name = query.get("name") ?? "";
  const op = query.get("op") ?? "";
  const period: Period = isPeriod(query.get("period")) ? (query.get("period") as Period) : "7d";
  const env = query.get("environment") ?? "";
  const samples = query.get("samples") === "recent" ? "recent" : "slowest";
  const params = new URLSearchParams({ name, op, period, samples });
  if (env) params.set("environment", env);
  const { data, error, reload } = useApi<TransactionDetail>(`/api/ui/projects/${projectId}/transactions/summary?${params}`);

  const [selected, setSelected] = useState<string | null>(query.get("sample"));
  const list = data?.samples ?? [];
  useEffect(() => {
    if (!selected && list[0]) setSelected(list[0].event_id);
  }, [list, selected]);
  const sample = useApi<TransactionPayload>(selected ? `/api/ui/projects/${projectId}/transactions/events/${selected}` : null);

  function set(key: string, value: string) {
    const next = new URLSearchParams(query);
    if (value) next.set(key, value);
    else next.delete(key);
    next.delete("sample");
    setSelected(null);
    navigate(`/p/${projectId}/performance/transaction?${next}`, { replace: true });
  }
  const back = () => {
    const q = new URLSearchParams();
    for (const k of ["period", "environment"]) if (query.get(k)) q.set(k, query.get(k)!);
    navigate(`/p/${projectId}/performance${q.size ? `?${q}` : ""}`);
  };
  const index = list.findIndex((s) => s.event_id === selected);
  useShortcuts({
    Escape: back,
    "[": () => index > 0 && setSelected(list[index - 1]!.event_id),
    "]": () => index < list.length - 1 && setSelected(list[index + 1]!.event_id),
  });

  const s = data?.summary;
  const tx = sample.data?.transaction;
  return (
    <div className="page">
      <div>
        <Link to={`/p/${projectId}/performance`} onClick={(e) => (e.preventDefault(), back())} className="faint" style={{ fontSize: "var(--text-sm)" }}>
          ← Performance <kbd>Esc</kbd>
        </Link>
      </div>
      <div className="page-head">
        <h1 className="mono" style={{ overflowWrap: "anywhere", fontSize: "var(--text-xl)" }}>
          {name}
        </h1>
        <span className="chip brand">{opLabels[op] ?? op}</span>
      </div>
      <PeriodFilter period={period} onPeriod={(p) => set("period", p === "7d" ? "" : p)} environments={[]} env={env} onEnv={(e) => set("environment", e)} />

      {error && (
        <div className="banner warn" role="alert">
          {error.status === 404 ? "None of these in this period. Try a longer one." : error.message}{" "}
          {error.status !== 404 && (
            <button className="btn small" onClick={() => reload()}>
              Retry
            </button>
          )}
        </div>
      )}

      {s && data && (
        <>
          <div className="stat-grid">
            <div className="stat">
              <span className="label">Typical (p50)</span>
              <span className="value hero">{duration(s.p50)}</span>
              <span className="sub">half are faster than this</span>
            </div>
            <div className="stat">
              <span className="label">Slow end (p95)</span>
              <span className="value hero">{duration(s.p95)}</span>
              <span className="sub">1 in 20 take longer</span>
            </div>
            <div className="stat">
              <span className="label">p75</span>
              <span className="value">{duration(s.p75)}</span>
            </div>
            <div className="stat">
              <span className="label">Count</span>
              <span className="value">{compact(s.count)}</span>
              <span className="sub">{s.failed ? `${compact(s.failed)} didn't finish ok` : "all finished ok"}</span>
            </div>
          </div>

          <section className="panel" style={{ padding: "var(--space-4)", display: "grid", gap: "var(--space-3)" }}>
            <h2 style={{ margin: 0, fontSize: "var(--text-md)" }}>Duration per {period === "24h" ? "hour" : "day"}</h2>
            <LineChart
              title={`Duration per ${period === "24h" ? "hour" : "day"}: p95 and p50`}
              labels={data.series.map((p) => bucketLabel(p.time, period))}
              series={[
                { key: "p95", label: "p95 (slow end)", values: data.series.map((p) => p.p95), tone: "accent" },
                { key: "p50", label: "p50 (typical)", values: data.series.map((p) => p.p50), tone: "context" },
              ]}
              format={duration}
              axisFormat={durationTick}
              extraRows={{ label: "count", value: (i) => compact(data.series[i]?.count ?? 0) }}
            />
          </section>

          <div className="grid-2">
            <section className="panel" style={{ overflowX: "auto" }}>
              <table className="table">
                <caption className="sr-only">Where the time goes</caption>
                <thead>
                  <tr>
                    <th>Where the time goes</th>
                    <th className="num">p50</th>
                    <th className="num">p95</th>
                    <th className="num" title="Total time in this kind of step, divided by the number of transactions">
                      Per {opLabels[op]?.toLowerCase() ?? "transaction"}
                    </th>
                  </tr>
                </thead>
                <tbody>
                  {data.span_ops.map((o) => (
                    <tr key={o.op}>
                      <td>{opLabels[o.op] ?? o.op}</td>
                      <td className="num">{duration(o.p50)}</td>
                      <td className="num">{duration(o.p95)}</td>
                      <td className="num">{duration(o.per_transaction)}</td>
                    </tr>
                  ))}
                  {data.span_ops.length === 0 && (
                    <tr>
                      <td colSpan={4} className="muted">
                        No spans recorded inside these.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </section>
            <section className="panel" style={{ overflowX: "auto" }}>
              <table className="table">
                <caption className="sr-only">By release</caption>
                <thead>
                  <tr>
                    <th>By release</th>
                    <th className="num">Count</th>
                    <th className="num">p50</th>
                    <th className="num">p95</th>
                  </tr>
                </thead>
                <tbody>
                  {data.releases.map((r) => (
                    <tr key={r.release}>
                      <td className="mono nowrap">
                        {r.release || <span className="faint">(no release)</span>}
                      </td>
                      <td className="num">{compact(r.count)}</td>
                      <td className="num">{duration(r.p50)}</td>
                      <td className="num">{duration(r.p95)}</td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </section>
          </div>

          <section className="panel" style={{ padding: "var(--space-4)", display: "grid", gap: "var(--space-3)" }}>
            <div className="page-head">
              <h2 style={{ margin: 0, fontSize: "var(--text-md)" }}>Samples</h2>
              <div className="seg" role="group" aria-label="Which samples">
                <button aria-pressed={samples === "slowest"} onClick={() => set("samples", "")}>
                  Slowest
                </button>
                <button aria-pressed={samples === "recent"} onClick={() => set("samples", "recent")}>
                  Most recent
                </button>
              </div>
            </div>
            {/* The selected sample first: its waterfall is what people come for, and on a phone it would
                otherwise sit below the whole list. */}
            {tx && sample.data && (
              <div style={{ display: "grid", gap: "var(--space-3)" }} aria-live="polite">
                <h3 style={{ margin: 0, fontSize: "var(--text-md)" }}>
                  Selected sample: {duration(tx.duration_ms)}{" "}
                  <span className="faint mono" style={{ fontSize: "var(--text-xs)" }}>
                    {sample.data.event_id}
                  </span>
                </h3>
                <Waterfall spans={tx.spans ?? []} total={tx.duration_ms} />
                {tx.measurements && Object.keys(tx.measurements).length > 0 && (
                  <dl className="kv">
                    {Object.entries(tx.measurements).map(([k, v]) => (
                      <div key={k} style={{ display: "contents" }}>
                        {/* from_*: which start point an app start was measured from (process, bundle or init). */}
                        <dt>{k.startsWith("from_") ? "Measured from" : (measurementLabels[k] ?? k.replace(/_/g, " "))}</dt>
                        <dd>{k.startsWith("from_") ? k.slice(5).replace(/_/g, " ") : duration(v)}</dd>
                      </div>
                    ))}
                  </dl>
                )}
              </div>
            )}
            <div style={{ overflowX: "auto" }}>
              <table className="table">
                <thead>
                  <tr>
                    <th className="num">Duration</th>
                    <th>Release</th>
                    <th className="hide-narrow">Environment</th>
                    <th className="num hide-narrow">Spans</th>
                    <th>When</th>
                  </tr>
                </thead>
                <tbody>
                  {list.map((x) => (
                    <tr key={x.event_id} className="clickable" aria-selected={x.event_id === selected} onClick={() => setSelected(x.event_id)}>
                      <td className="num">
                        {duration(x.duration_ms)} {x.status !== "ok" && <span className="chip unresolved">{x.status === "deadline_exceeded" ? "Timed out" : x.status}</span>}
                      </td>
                      <td className="mono nowrap">
                        {x.release || "–"}
                      </td>
                      <td className="hide-narrow">{x.environment}</td>
                      <td className="num hide-narrow">{x.spans}</td>
                      <td className="faint" title={exact(x.started_at)}>
                        {relative(x.started_at)}
                      </td>
                    </tr>
                  ))}
                </tbody>
              </table>
            </div>
            <p className="faint" style={{ fontSize: "var(--text-xs)", margin: 0 }}>
              <kbd>[</kbd> <kbd>]</kbd> previous / next sample
            </p>

          </section>
        </>
      )}
    </div>
  );
}
