import { Empty, LineChart } from "../components/charts";
import type { Period, ReleaseHealth } from "../lib/api";
import { navigate, useLocation } from "../lib/router";
import { bucketLabel, isPeriod, percent, percentTick } from "../lib/perf";
import { compact, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";
import { PeriodFilter } from "./Performance";

// Release health: how often people's sessions end in a crash, per release.
// A session is one run of the app in the foreground (one page load on the
// web). "Crash-free sessions" is the share that didn't end in a crash, the
// number mobile teams watch after every release.

export function Releases({ projectId }: { projectId: number }) {
  const { query } = useLocation();
  const period: Period = isPeriod(query.get("period")) ? (query.get("period") as Period) : "7d";
  const env = query.get("environment") ?? "";
  const params = new URLSearchParams({ period });
  if (env) params.set("environment", env);
  // Sessions arrive all the time; refresh every 30 s while the tab is open.
  const { data, error, loading, reload } = useApi<ReleaseHealth>(`/api/ui/projects/${projectId}/releases?${params}`, 30_000);

  function set(key: string, value: string) {
    const next = new URLSearchParams(query);
    if (value) next.set(key, value);
    else next.delete(key);
    navigate(`/p/${projectId}/releases${next.size ? `?${next}` : ""}`, { replace: true });
  }

  const t = data?.totals;
  const series = data?.series ?? [];
  const rates = series.map((p) => (p.sessions > 0 ? 1 - p.crashed / p.sessions : null));
  const known = rates.filter((r): r is number => r !== null);
  // Start the axis just below the lowest point: between 97 and 100 % is
  // where the story is, and from 0 it would be a flat line at the top.
  const yMin = known.length ? Math.max(0, Math.floor(Math.min(...known) * 20) / 20 - 0.05) : 0.9;

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Releases</h1>
          <p className="muted" style={{ margin: 0 }}>
            How often a session ends in a crash, for each version of your app.
          </p>
        </div>
      </div>
      <PeriodFilter period={period} onPeriod={(p) => set("period", p === "7d" ? "" : p)} environments={data?.environments ?? []} env={env} onEnv={(e) => set("environment", e)} />

      {error && (
        <div className="banner warn" role="alert">
          {error.message}{" "}
          <button className="btn small" onClick={() => reload()}>
            Retry
          </button>
        </div>
      )}

      {t && t.sessions === 0 && !env ? (
        <Empty title="No sessions yet">
          <p>
            The juju SDKs count sessions on their own: a page load on the web, a run of the app in the foreground on mobile. Update to the latest SDK and open your app, and sessions show up here within a
            minute.
          </p>
          <p>Each session that ends in an uncaught error or a fatal crash lowers the release's crash-free rate.</p>
        </Empty>
      ) : (
        t && (
          <div style={{ display: "grid", gap: "var(--space-4)", opacity: loading ? 0.6 : 1 }} aria-busy={loading}>
            <div className="stat-grid">
              <div className="stat">
                <span className="label">Crash-free sessions</span>
                <span className="value hero">{percent(t.crash_free_sessions)}</span>
                <span className="sub">
                  {compact(t.sessions - t.crashed)} of {compact(t.sessions)} sessions didn't crash
                </span>
              </div>
              <div className="stat">
                <span className="label">Crash-free users</span>
                <span className="value hero">{percent(t.crash_free_users)}</span>
                <span className="sub">{t.users ? `${compact(t.users - t.crashed_users)} of ${compact(t.users)} known users` : "Call setUser to count people, not just sessions"}</span>
              </div>
              <div className="stat">
                <span className="label">Crashed</span>
                <span className="value">{compact(t.crashed)}</span>
                <span className="sub">sessions that ended in a crash</span>
              </div>
              <div className="stat">
                <span className="label">With errors</span>
                <span className="value">{compact(t.errored)}</span>
                <span className="sub">had an error, but didn't crash</span>
              </div>
            </div>

            <section className="panel" style={{ padding: "var(--space-4)", display: "grid", gap: "var(--space-3)" }}>
              <h2 style={{ margin: 0, fontSize: "var(--text-md)" }}>Crash-free sessions per {period === "24h" ? "hour" : "day"}</h2>
              <LineChart
                title={`Crash-free sessions per ${period === "24h" ? "hour" : "day"}`}
                labels={series.map((p) => bucketLabel(p.time, period))}
                series={[{ key: "crash-free", label: "Crash-free sessions", values: rates, tone: "accent" }]}
                format={(v) => percent(v)}
                axisFormat={percentTick}
                yMin={yMin}
                extraRows={{ label: "sessions (crashed)", value: (i) => (series[i] ? `${compact(series[i]!.sessions)} (${compact(series[i]!.crashed)})` : "") }}
              />
            </section>

            <section className="panel" style={{ overflowX: "auto" }}>
              <table className="table">
                <thead>
                  <tr>
                    <th>Release</th>
                    <th className="num">Crash-free sessions</th>
                    <th className="num hide-narrow">Crash-free users</th>
                    <th className="num">Sessions</th>
                    <th className="num">Crashed</th>
                    <th className="hide-narrow">Adoption</th>
                    <th className="hide-narrow">Last session</th>
                  </tr>
                </thead>
                <tbody>
                  {data!.releases.map((r) => (
                    <tr key={r.release}>
                      <td className="mono nowrap">
                        {r.release || <span className="faint">(no release set)</span>}
                      </td>
                      <td className="num">{percent(r.crash_free_sessions)}</td>
                      <td className="num hide-narrow">{percent(r.crash_free_users)}</td>
                      <td className="num">{compact(r.sessions)}</td>
                      <td className="num">{compact(r.crashed)}</td>
                      <td className="hide-narrow" title={`${percent(r.adoption)} of all sessions in this period`}>
                        <div style={{ display: "flex", gap: 8, alignItems: "center" }}>
                          <div className="meter" style={{ flex: 1 }} aria-hidden="true">
                            <span style={{ width: `${r.adoption * 100}%` }} />
                          </div>
                          <span className="rate faint">{Math.round(r.adoption * 100)}%</span>
                        </div>
                      </td>
                      <td className="hide-narrow faint" title={exact(r.last_seen)}>
                        {relative(r.last_seen)}
                      </td>
                    </tr>
                  ))}
                  {data!.releases.length === 0 && (
                    <tr>
                      <td colSpan={7} className="muted">
                        No sessions in this period{env ? ` for ${env}` : ""}.
                      </td>
                    </tr>
                  )}
                </tbody>
              </table>
            </section>
          </div>
        )
      )}
    </div>
  );
}
