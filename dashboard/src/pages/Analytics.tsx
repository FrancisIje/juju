import { useEffect, useMemo, useState } from "react";
import { Code } from "../components/CopyButton";
import { LineChart } from "../components/LineChart";
import { Pixels } from "../components/Pixels";
import { breakdowns, joinSteps, percent, ranges, seriesLabel, splitSteps, type Cohort, type FunnelStep, type Summary, type Trend } from "../lib/analytics";
import type { ProjectInfo } from "../lib/api";
import { useShortcuts } from "../lib/keys";
import { Link, navigate, useLocation } from "../lib/router";
import { captureSnippet, platforms, type Platform } from "../lib/snippets";
import { compact } from "../lib/time";
import { useApi } from "../lib/useApi";

export type AnalyticsTab = "trends" | "funnels" | "retention";
const tabs: { id: AnalyticsTab; label: string }[] = [
  { id: "trends", label: "Trends" },
  { id: "funnels", label: "Funnels" },
  { id: "retention", label: "Retention" },
];

// Product analytics: what people do in your apps. Every choice lives in the
// URL, so a chart can be bookmarked or sent to a teammate.
export function Analytics({ projectId, tab }: { projectId: number; tab: AnalyticsTab }) {
  const base = `/p/${projectId}/analytics`;
  const summary = useApi<Summary>(`/api/ui/projects/${projectId}/analytics?days=90`);
  useShortcuts({
    "1": () => navigate(`${base}/trends`),
    "2": () => navigate(`${base}/funnels`),
    "3": () => navigate(`${base}/retention`),
  });

  // Refresh has to reach the open tab too, or the chart stays as it was.
  const [refresh, setRefresh] = useState(0);
  const s = summary.data;
  const events = (s?.events ?? []).filter((e) => !e.name.startsWith("$"));
  return (
    <div className="page">
      <div className="page-head">
        <h1>Analytics</h1>
        {s?.enabled && events.length > 0 && (
          <button
            className="btn small"
            onClick={() => {
              void summary.reload();
              setRefresh((n) => n + 1);
            }}
            title="Load the newest events"
          >
            Refresh
          </button>
        )}
      </div>
      {summary.error && (
        <div className="banner warn" role="alert">
          {summary.error.message}{" "}
          <button className="btn small" onClick={() => summary.reload()}>
            Retry
          </button>
        </div>
      )}
      {!s && !summary.error && <p className="muted">Loading…</p>}
      {s && !s.enabled && <Off />}
      {s?.enabled && events.length === 0 && <FirstEvent projectId={projectId} />}
      {s?.enabled && events.length > 0 && (
        <>
          <div className="tabs" role="tablist" aria-label="Analytics">
            {tabs.map((t, i) => (
              <Link key={t.id} to={`${base}/${t.id}`} role="tab" aria-selected={tab === t.id} title={`Shortcut: ${i + 1}`}>
                {t.label}
              </Link>
            ))}
          </div>
          {tab === "trends" && <Trends projectId={projectId} events={events.map((e) => e.name)} refresh={refresh} />}
          {tab === "funnels" && <Funnels projectId={projectId} events={events.map((e) => e.name)} refresh={refresh} />}
          {tab === "retention" && <Retention projectId={projectId} events={events.map((e) => e.name)} refresh={refresh} />}
        </>
      )}
    </div>
  );
}

interface TabProps {
  projectId: number;
  events: string[];
  refresh: number;
}

function Off() {
  return (
    <section className="panel onboard">
      <div style={{ display: "grid", gap: 4 }}>
        <span className="chip" style={{ justifySelf: "start" }}>
          Off
        </span>
        <h2>Analytics needs ClickHouse</h2>
        <p className="muted">
          Analytics events go to ClickHouse, a database built for counting large numbers of events over time. Error tracking works without it. Whoever runs this server can turn
          analytics on:
        </p>
      </div>
      <Code>{`# locally: run ClickHouse (no Docker needed), then start juju with
make devch
JUJU_CLICKHOUSE_URL=http://127.0.0.1:8123 make dev

# on a server set up with deploy/setup.sh it's already installed;
# see JUJU_CLICKHOUSE_URL in /etc/juju/juju.env`}</Code>
    </section>
  );
}

function FirstEvent({ projectId }: { projectId: number }) {
  const project = useApi<{ project: ProjectInfo }>(`/api/ui/projects/${projectId}`);
  const [platform, setPlatform] = useState<Platform>("web");
  return (
    <section className="panel onboard" aria-labelledby="first-event">
      <div style={{ display: "grid", gap: 4 }}>
        <span className="chip brand" style={{ justifySelf: "start" }}>
          Waiting for the first event
        </span>
        <h2 id="first-event">Track what people do</h2>
        <p className="muted">
          Call <code className="mono">capture</code> when something worth counting happens: a sign-up, a checkout, a feature used. It uses the same SDK and DSN as error tracking.
        </p>
      </div>
      <div className="tabs" role="tablist" aria-label="Platform">
        {platforms.map((p) => (
          <button key={p.id} role="tab" aria-selected={platform === p.id} onClick={() => setPlatform(p.id)}>
            {p.label}
          </button>
        ))}
      </div>
      <Code>{captureSnippet(platform, project.data?.project.dsn ?? "…")}</Code>
      <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
        Until identify(), events count by an anonymous id kept on the device. After identify(), its earlier events count for that user too.
      </p>
    </section>
  );
}

/** Reloads these requests when the page's Refresh button is pressed. */
function useRefresh(refresh: number, ...requests: { reload: (quiet?: boolean) => Promise<void> }[]) {
  useEffect(() => {
    if (refresh > 0) for (const r of requests) void r.reload(true);
    // Only a press of the button: the requests change on every render.
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [refresh]);
}

/** Reads and writes one URL query parameter, so the view can be shared. */
function useParam(key: string, fallback: string): [string, (v: string) => void] {
  const { path, query } = useLocation();
  const value = query.get(key) ?? fallback;
  const set = (v: string) => {
    const next = new URLSearchParams(query);
    if (v === fallback) next.delete(key);
    else next.set(key, v);
    navigate(path + (next.size ? `?${next}` : ""), { replace: true });
  };
  return [value, set];
}

function Trends({ projectId, events, refresh }: TabProps) {
  const [event, setEvent] = useParam("event", events[0]!);
  const [rangeId, setRange] = useParam("range", "7d");
  const [math, setMath] = useParam("math", "events");
  const [breakdown, setBreakdown] = useParam("breakdown", "");
  const range = ranges.find((r) => r.id === rangeId) ?? ranges[1]!;
  const props = useApi<{ properties: string[] }>(`/api/ui/projects/${projectId}/analytics/properties?event=${encodeURIComponent(event)}`);
  const q = new URLSearchParams({ event, days: String(range.days), interval: range.interval, math, breakdown });
  const trend = useApi<Trend>(`/api/ui/projects/${projectId}/analytics/trend?${q}`, 30_000);
  const t = trend.data;
  const series = useMemo(() => (t?.series ?? []).map((s) => ({ ...s, label: seriesLabel(breakdown, s.label) })), [t, breakdown]);
  useRefresh(refresh, trend, props);
  const total = t?.total ?? 0;

  return (
    <div style={{ display: "grid", gap: "var(--space-4)" }}>
      <div className="controls">
        <label className="sr-only" htmlFor="trend-event">
          Event
        </label>
        <select id="trend-event" className="select" value={event} onChange={(e) => setEvent(e.target.value)}>
          {(events.includes(event) ? events : [event, ...events]).map((n) => (
            <option key={n}>{n}</option>
          ))}
        </select>
        <div className="seg" role="group" aria-label="Count">
          <button aria-pressed={math === "events"} onClick={() => setMath("events")} title="Every time it happened">
            Events
          </button>
          <button aria-pressed={math === "people"} onClick={() => setMath("people")} title="How many different people did it">
            People
          </button>
        </div>
        <label className="sr-only" htmlFor="trend-breakdown">
          Break down by
        </label>
        <select id="trend-breakdown" className="select" value={breakdown} onChange={(e) => setBreakdown(e.target.value)}>
          {breakdowns.map((b) => (
            <option key={b.id} value={b.id}>
              {b.id ? `By ${b.label.toLowerCase()}` : b.label}
            </option>
          ))}
          {(props.data?.properties ?? []).length > 0 && (
            <optgroup label="By property">
              {props.data!.properties.map((p) => (
                <option key={p} value={`property:${p}`}>
                  {p}
                </option>
              ))}
            </optgroup>
          )}
        </select>
        <label className="sr-only" htmlFor="trend-range">
          Time range
        </label>
        <select id="trend-range" className="select" value={range.id} onChange={(e) => setRange(e.target.value)}>
          {ranges.map((r) => (
            <option key={r.id} value={r.id}>
              {r.label}
            </option>
          ))}
        </select>
      </div>

      {trend.error && (
        <div className="banner warn" role="alert">
          {trend.error.message}
        </div>
      )}
      <section className="panel" style={{ padding: "var(--space-4)", display: "grid", gap: "var(--space-3)" }} aria-busy={trend.loading}>
        <div style={{ display: "flex", alignItems: "baseline", gap: "var(--space-3)", flexWrap: "wrap" }}>
          <span className="mono" style={{ fontSize: "var(--text-xl)", fontWeight: 500 }}>
            {compact(total)}
          </span>
          <span className="muted">
            {math === "people" ? (total === 1 ? "person did" : "people did") : "times:"} <strong className="mono">{event}</strong>, {range.label.toLowerCase()}
          </span>
        </div>
        {t && <LineChart buckets={t.buckets} series={series} interval={range.interval} />}
        <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
          Grouped by {range.interval} in UTC. {math === "people" ? "The total counts each person once for the whole range; each point counts who did it that " + range.interval + ". identify() links a person's devices." : "A retried event counts once."}
        </p>
      </section>

      {t && (
        <section className="panel" style={{ overflow: "hidden" }}>
          <div style={{ overflowX: "auto" }}>
            <table className="table">
              <thead>
                <tr>
                  <th>{breakdown ? (breakdowns.find((b) => b.id === breakdown)?.label ?? breakdown.replace("property:", "")) : "Series"}</th>
                  <th className="num">{math === "people" ? "People" : "Events"}</th>
                  <th className="num">Share</th>
                </tr>
              </thead>
              <tbody>
                {series.map((s, i) => (
                  <tr key={s.label + i}>
                    <td>
                      <span className="swatch" style={{ background: `var(--chart-${(i % 10) + 1})`, marginRight: 8 }} aria-hidden="true" />
                      {s.label}
                    </td>
                    <td className="num mono">{s.total.toLocaleString()}</td>
                    <td className="num mono">{percent(s.total, total)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          {t.hidden > 0 && (
            <p className="faint" style={{ padding: "var(--space-2) var(--space-3)", fontSize: "var(--text-xs)" }}>
              {t.hidden} smaller value{t.hidden === 1 ? "" : "s"} not shown. The chart shows the top 10.
            </p>
          )}
        </section>
      )}
    </div>
  );
}

function Funnels({ projectId, events, refresh }: TabProps) {
  const [stepsParam, setStepsParam] = useParam("steps", joinSteps(events.slice(0, 2)));
  const [windowHours, setWindow] = useParam("window", "24");
  const [days, setDays] = useParam("days", "30");
  const steps = splitSteps(stepsParam);
  const setSteps = (next: string[]) => setStepsParam(joinSteps(next));
  const valid = steps.length >= 2;
  const q = new URLSearchParams({ window_hours: windowHours, days });
  for (const s of steps) q.append("step", s);
  const funnel = useApi<{ steps: FunnelStep[] }>(valid ? `/api/ui/projects/${projectId}/analytics/funnel?${q}` : null);
  useRefresh(refresh, funnel);
  const setStep = (i: number, v: string) => setSteps(steps.map((s, j) => (j === i ? v : s)));
  const first = funnel.data?.steps[0]?.people ?? 0;

  return (
    <div style={{ display: "grid", gap: "var(--space-4)" }}>
      <p className="muted">How many people did these steps in this order, each within the time window of their first step.</p>
      <section className="panel" style={{ padding: "var(--space-4)", display: "grid", gap: "var(--space-3)" }}>
        <ol style={{ margin: 0, paddingLeft: "1.4em", display: "grid", gap: "var(--space-2)" }}>
          {steps.map((s, i) => (
            <li key={i}>
              <div className="controls">
                <label className="sr-only" htmlFor={`step-${i}`}>
                  Step {i + 1}
                </label>
                <select id={`step-${i}`} className="select" value={s} onChange={(e) => setStep(i, e.target.value)}>
                  {(events.includes(s) ? events : [s, ...events]).map((n) => (
                    <option key={n}>{n}</option>
                  ))}
                </select>
                {steps.length > 2 && (
                  <button className="btn ghost small" onClick={() => setSteps(steps.filter((_, j) => j !== i))} aria-label={`Remove step ${i + 1}`}>
                    Remove
                  </button>
                )}
              </div>
            </li>
          ))}
        </ol>
        <div className="controls">
          {steps.length < 8 && (
            <button className="btn small" onClick={() => setSteps([...steps, events.find((e) => !steps.includes(e)) ?? events[0]!])}>
              + Add a step
            </button>
          )}
          <label className="sr-only" htmlFor="funnel-window">
            Conversion window
          </label>
          <select id="funnel-window" className="select" value={windowHours} onChange={(e) => setWindow(e.target.value)}>
            <option value="1">within 1 hour</option>
            <option value="24">within 1 day</option>
            <option value="168">within 7 days</option>
            <option value="720">within 30 days</option>
          </select>
          <label className="sr-only" htmlFor="funnel-days">
            Time range
          </label>
          <select id="funnel-days" className="select" value={days} onChange={(e) => setDays(e.target.value)}>
            <option value="7">Last 7 days</option>
            <option value="30">Last 30 days</option>
            <option value="90">Last 90 days</option>
          </select>
        </div>
      </section>
      {funnel.error && (
        <div className="banner warn" role="alert">
          {funnel.error.message}
        </div>
      )}
      {funnel.data && (
        <section className="panel" aria-label="Funnel result" style={{ overflow: "hidden" }}>
          {funnel.data.steps.map((st, i) => {
            const prev = i === 0 ? st.people : funnel.data!.steps[i - 1]!.people;
            return (
              <div className="funnel-step" key={i}>
                <span>
                  <span className="faint mono">{i + 1}.</span> <strong className="mono">{st.event}</strong>
                </span>
                <span className="mono">
                  {st.people.toLocaleString()} {st.people === 1 ? "person" : "people"} · {percent(st.people, first)}
                </span>
                <div className="funnel-bar" aria-hidden="true">
                  <span style={{ width: first ? `${(st.people / first) * 100}%` : 0 }} />
                </div>
                {i > 0 && (
                  <span className="faint" style={{ gridColumn: "1 / -1", fontSize: "var(--text-xs)" }}>
                    {percent(st.people, prev)} of step {i}
                    {prev - st.people > 0 ? `, ${(prev - st.people).toLocaleString()} dropped off` : ""}
                  </span>
                )}
              </div>
            );
          })}
        </section>
      )}
    </div>
  );
}

function Retention({ projectId, events, refresh }: TabProps) {
  const [start, setStart] = useParam("start", events[0]!);
  const [ret, setRet] = useParam("return", events[1] ?? events[0]!);
  const [days, setDays] = useParam("days", "14");
  const q = new URLSearchParams({ start, return: ret, days, periods: "7" });
  const res = useApi<{ cohorts: Cohort[]; periods: number }>(`/api/ui/projects/${projectId}/analytics/retention?${q}`);
  useRefresh(refresh, res);
  const periods = res.data?.periods ?? 7;
  const cohorts = (res.data?.cohorts ?? []).filter((c) => c.people > 0);

  return (
    <div style={{ display: "grid", gap: "var(--space-4)" }}>
      <p className="muted">Of the people who did the first event on a day, how many came back and did the second on each following day.</p>
      <div className="controls">
        <span className="muted">People who did</span>
        <select aria-label="Starting event" className="select" value={start} onChange={(e) => setStart(e.target.value)}>
          {(events.includes(start) ? events : [start, ...events]).map((n) => (
            <option key={n}>{n}</option>
          ))}
        </select>
        <span className="muted">then came back to</span>
        <select aria-label="Returning event" className="select" value={ret} onChange={(e) => setRet(e.target.value)}>
          {(events.includes(ret) ? events : [ret, ...events]).map((n) => (
            <option key={n}>{n}</option>
          ))}
        </select>
        <select aria-label="Cohorts" className="select" value={days} onChange={(e) => setDays(e.target.value)}>
          <option value="7">Last 7 days</option>
          <option value="14">Last 14 days</option>
          <option value="30">Last 30 days</option>
        </select>
      </div>
      {res.error && (
        <div className="banner warn" role="alert">
          {res.error.message}
        </div>
      )}
      {res.data && cohorts.length === 0 && (
        <div className="panel empty">
          <Pixels seed={5} density={0.3} cell={12} />
          <h2>Nobody did {start} in this range</h2>
          <p className="muted">Choose another starting event or a longer range.</p>
        </div>
      )}
      {cohorts.length > 0 && (
        <section className="panel" style={{ overflow: "hidden" }}>
          <div style={{ overflowX: "auto" }}>
            <table className="table retention">
              <thead>
                <tr>
                  <th>Started</th>
                  <th className="num">People</th>
                  {Array.from({ length: periods }, (_, i) => (
                    <th key={i} className="num">
                      Day {i + 1}
                    </th>
                  ))}
                </tr>
              </thead>
              <tbody>
                {cohorts.map((c) => (
                  <tr key={c.day}>
                    <td>{new Date(c.day).toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" })}</td>
                    <td className="num mono">{c.people.toLocaleString()}</td>
                    {c.returned.map((r, i) => {
                      const future = new Date(c.day).getTime() + (i + 1) * 864e5 > Date.now();
                      const share = c.people ? r / c.people : 0;
                      return (
                        <td
                          key={i}
                          className="cell"
                          title={future ? "Hasn't happened yet" : `${r} of ${c.people} came back on day ${i + 1}`}
                          style={future ? undefined : { background: `color-mix(in srgb, var(--chart-1) ${Math.round(share * 60)}%, transparent)` }}
                        >
                          {future ? <span className="faint">·</span> : percent(r, c.people)}
                        </td>
                      );
                    })}
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
          <p className="faint" style={{ padding: "var(--space-2) var(--space-3)", fontSize: "var(--text-xs)" }}>
            Days in UTC. A dot is a day that hasn't happened yet.
          </p>
        </section>
      )}
    </div>
  );
}
