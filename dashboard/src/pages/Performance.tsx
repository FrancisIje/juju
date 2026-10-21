import { useEffect, useState } from "react";
import { Empty } from "../components/charts";
import type { Period, TransactionList } from "../lib/api";
import { useShortcuts } from "../lib/keys";
import { navigate, useLocation } from "../lib/router";
import { duration, isPeriod, opLabels, periods } from "../lib/perf";
import { compact, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";

// Performance: every kind of transaction (page loads, app starts, screen
// loads), how often it happens and how long it takes.

export function PeriodFilter({
  period,
  onPeriod,
  environments,
  env,
  onEnv,
  children,
}: {
  period: Period;
  onPeriod: (p: Period) => void;
  environments: string[];
  env: string;
  onEnv: (e: string) => void;
  children?: React.ReactNode;
}) {
  return (
    <div className="filters" role="group" aria-label="Filters">
      <div className="seg" role="group" aria-label="Period">
        {periods.map((p) => (
          <button key={p.id} aria-pressed={period === p.id} onClick={() => onPeriod(p.id)} title={`Last ${p.label}`}>
            {p.id}
          </button>
        ))}
      </div>
      {(environments.length > 1 || env) && (
        <select className="select" style={{ width: "auto" }} aria-label="Environment" value={env} onChange={(e) => onEnv(e.target.value)}>
          <option value="">All environments</option>
          {environments.map((e) => (
            <option key={e}>{e}</option>
          ))}
        </select>
      )}
      {children}
    </div>
  );
}

const sorts = [
  { id: "count", label: "Most frequent" },
  { id: "p95", label: "Slowest (p95)" },
  { id: "p50", label: "Slowest (p50)" },
  { id: "name", label: "Name" },
];

export function transactionLink(projectId: number, name: string, op: string, query: URLSearchParams) {
  const q = new URLSearchParams({ name, op });
  for (const k of ["period", "environment"]) if (query.get(k)) q.set(k, query.get(k)!);
  return `/p/${projectId}/performance/transaction?${q}`;
}

export function Performance({ projectId }: { projectId: number }) {
  const { query } = useLocation();
  const period: Period = isPeriod(query.get("period")) ? (query.get("period") as Period) : "7d";
  const env = query.get("environment") ?? "";
  const op = query.get("op") ?? "";
  const sort = query.get("sort") ?? "count";
  const params = new URLSearchParams({ period, sort });
  if (env) params.set("environment", env);
  if (op) params.set("op", op);
  const { data, error, loading, reload } = useApi<TransactionList>(`/api/ui/projects/${projectId}/transactions?${params}`, 30_000);
  const [focus, setFocus] = useState(0);
  const rows = data?.transactions ?? [];
  useEffect(() => setFocus((f) => Math.min(f, Math.max(0, rows.length - 1))), [rows.length]);

  function set(key: string, value: string) {
    const next = new URLSearchParams(query);
    if (value) next.set(key, value);
    else next.delete(key);
    navigate(`/p/${projectId}/performance${next.size ? `?${next}` : ""}`, { replace: true });
  }
  const open = (i: number) => rows[i] && navigate(transactionLink(projectId, rows[i]!.name, rows[i]!.op, query));
  useShortcuts({
    j: () => setFocus((f) => Math.min(rows.length - 1, f + 1)),
    k: () => setFocus((f) => Math.max(0, f - 1)),
    ArrowDown: () => setFocus((f) => Math.min(rows.length - 1, f + 1)),
    ArrowUp: () => setFocus((f) => Math.max(0, f - 1)),
    Enter: () => open(focus),
    o: () => open(focus),
  });

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Performance</h1>
          <p className="muted" style={{ margin: 0 }}>
            How long page loads, app starts and screens take. p50 is the typical time; p95 is what the slowest 1 in 20 people wait.
          </p>
        </div>
      </div>
      <PeriodFilter period={period} onPeriod={(p) => set("period", p === "7d" ? "" : p)} environments={data?.environments ?? []} env={env} onEnv={(e) => set("environment", e)}>
        <select className="select" style={{ width: "auto" }} aria-label="Kind" value={op} onChange={(e) => set("op", e.target.value)}>
          <option value="">All kinds</option>
          {(data?.ops ?? []).map((o) => (
            <option key={o} value={o}>
              {opLabels[o] ?? o}
            </option>
          ))}
        </select>
        <select className="select" style={{ width: "auto" }} aria-label="Sort by" value={sort} onChange={(e) => set("sort", e.target.value === "count" ? "" : e.target.value)}>
          {sorts.map((s) => (
            <option key={s.id} value={s.id}>
              {s.label}
            </option>
          ))}
        </select>
      </PeriodFilter>

      {error && (
        <div className="banner warn" role="alert">
          {error.message}{" "}
          <button className="btn small" onClick={() => reload()}>
            Retry
          </button>
        </div>
      )}

      {data && rows.length === 0 && !op && !env ? (
        <Empty title="Nothing timed yet">
          <p>
            The juju SDKs time page loads (web), app starts and screen loads (React Native with <code>trackNavigation</code>, Flutter with <code>Juju.navigatorObserver</code>) by themselves, along with the
            HTTP requests made while they happen. Update to the latest SDK, open your app, and the first ones appear here within a minute.
          </p>
        </Empty>
      ) : (
        <section className="panel" style={{ overflowX: "auto", opacity: loading ? 0.6 : 1 }} aria-busy={loading}>
          <table className="table">
            <thead>
              <tr>
                <th>Transaction</th>
                <th className="num">Count</th>
                <th className="num">p50</th>
                <th className="num hide-narrow">p75</th>
                <th className="num">p95</th>
                <th className="num hide-narrow">Failed</th>
                <th className="hide-narrow">Last seen</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((r, i) => (
                <tr key={`${r.op} ${r.name}`} className="clickable" aria-selected={i === focus} onClick={() => open(i)} onMouseEnter={() => setFocus(i)}>
                  <td style={{ minWidth: 0 }}>
                    <a href={transactionLink(projectId, r.name, r.op, query)} onClick={(e) => e.preventDefault()} className="mono" style={{ overflowWrap: "anywhere", color: "var(--text-strong)" }}>
                      {r.name}
                    </a>{" "}
                    <span className="chip">{opLabels[r.op] ?? r.op}</span>
                  </td>
                  <td className="num">{compact(r.count)}</td>
                  <td className="num">{duration(r.p50)}</td>
                  <td className="num hide-narrow">{duration(r.p75)}</td>
                  <td className="num">{duration(r.p95)}</td>
                  <td className="num hide-narrow">{r.failed ? compact(r.failed) : "–"}</td>
                  <td className="hide-narrow faint" title={exact(r.last_seen)}>
                    {relative(r.last_seen)}
                  </td>
                </tr>
              ))}
              {rows.length === 0 && (
                <tr>
                  <td colSpan={7} className="muted">
                    Nothing matches these filters in this period.
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </section>
      )}
      <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
        <kbd>j</kbd> <kbd>k</kbd> move · <kbd>Enter</kbd> open · <kbd>?</kbd> all shortcuts
      </p>
    </div>
  );
}
