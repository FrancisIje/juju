import { useEffect, useState } from "react";
import { Breadcrumbs } from "../components/issue/Breadcrumbs";
import { Context } from "../components/issue/Context";
import { StackTrace } from "../components/issue/StackTrace";
import { Code } from "../components/CopyButton";
import { Sparkline } from "../components/Sparkline";
import { PlatformChip, StatusChip } from "../components/StatusChip";
import { api, ApiError, type Activity, type EventPayload, type IssueDetail, type Status } from "../lib/api";
import { useApp } from "../lib/app";
import { splitTitle } from "../lib/frames";
import { useShortcuts } from "../lib/keys";
import { Link, navigate } from "../lib/router";
import { compact, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";

type Tab = "stack" | "crumbs" | "context" | "json";
const tabs: { id: Tab; label: string }[] = [
  { id: "stack", label: "Stack trace" },
  { id: "crumbs", label: "Breadcrumbs" },
  { id: "context", label: "Context" },
  { id: "json", label: "Raw JSON" },
];

const PAGE = 20;

const activityText: Record<Activity["kind"], string> = {
  created: "First seen",
  resolved: "Resolved",
  ignored: "Ignored",
  reopened: "Marked unresolved",
  regressed: "Happened again after being resolved",
};

export function Issue({ projectId, issueId }: { projectId: number; issueId: number }) {
  const { toast, reloadMe } = useApp();
  const base = `/p/${projectId}/issues`;
  const issue = useApi<IssueDetail>(`/api/ui/projects/${projectId}/issues/${issueId}`, 30_000);
  const [events, setEvents] = useState<EventPayload[]>([]);
  const [more, setMore] = useState(true);
  const [index, setIndex] = useState(0);
  const [tab, setTab] = useState<Tab>("stack");
  const [showRaw, setShowRaw] = useState(false);

  // Events are fetched newest first, a page at a time. Moving past the last
  // loaded one fetches the next page, using its timestamp as the cursor.
  async function loadEvents(before?: string) {
    const q = new URLSearchParams({ limit: String(PAGE) });
    if (before) q.set("before", before);
    const r = await api<{ events: EventPayload[] }>(`/api/ui/projects/${projectId}/issues/${issueId}/events?${q}`);
    setMore(r.events.length === PAGE);
    setEvents((prev) => (before ? [...prev, ...r.events] : r.events));
    return r.events.length;
  }

  useEffect(() => {
    setEvents([]);
    setIndex(0);
    setMore(true);
    loadEvents().catch(() => setMore(false));
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId, issueId]);

  async function older() {
    if (index < events.length - 1) return setIndex(index + 1);
    const last = events[events.length - 1];
    if (!more || !last) return;
    if ((await loadEvents(last.timestamp)) > 0) setIndex(index + 1);
  }
  const newer = () => setIndex((i) => Math.max(0, i - 1));

  async function setStatus(next: Status) {
    const d = issue.data;
    if (!d || d.status === next) return;
    issue.setData({ ...d, status: next, regressed: false });
    try {
      await api(`/api/ui/projects/${projectId}/issues/status`, { method: "POST", body: { ids: [issueId], status: next } });
      toast(next === "resolved" ? "Resolved. You'll hear about it if it happens again." : next === "ignored" ? "Ignored. It won't alert you." : "Marked unresolved");
      void issue.reload(true);
      void reloadMe();
    } catch (err) {
      issue.setData(d);
      toast(err instanceof ApiError ? err.message : "Couldn't update the issue.");
    }
  }

  useShortcuts({
    r: () => void setStatus("resolved"),
    i: () => void setStatus("ignored"),
    u: () => void setStatus("unresolved"),
    "[": newer,
    "]": () => void older(),
    Escape: () => navigate(base),
    "1": () => setTab("stack"),
    "2": () => setTab("crumbs"),
    "3": () => setTab("context"),
    "4": () => setTab("json"),
  });

  if (issue.error) {
    return (
      <div className="page">
        <Link to={base} className="muted">
          ← Issues
        </Link>
        <div className="empty">
          <h2>{issue.error.status === 404 ? "This issue doesn't exist" : "Couldn't load this issue"}</h2>
          <p className="muted">{issue.error.status === 404 ? "It may be in another project, or the link is wrong." : issue.error.message}</p>
          {issue.error.status !== 404 && (
            <button className="btn" onClick={() => issue.reload()}>
              Retry
            </button>
          )}
        </div>
      </div>
    );
  }
  const d = issue.data;
  if (!d) return <div className="page muted" aria-busy="true">Loading…</div>;

  const [type, msg] = splitTitle(d.title);
  const event = events[index];
  const frames = event?.exception.stacktrace?.frames ?? [];
  const sym = event?.symbolication;
  const hasRaw = frames.some((f) => f.raw);

  return (
    <div className="page">
      <Link to={base} className="muted" style={{ fontSize: "var(--text-sm)" }}>
        ← Issues <kbd>Esc</kbd>
      </Link>

      <header style={{ display: "grid", gap: "var(--space-2)" }}>
        <div className="issue-meta">
          <StatusChip status={d.status} regressed={d.regressed} />
          <PlatformChip platform={d.platform} />
          <span className="mono faint">#{d.id}</span>
        </div>
        <h1 style={{ overflowWrap: "anywhere" }}>
          {type}
          {msg && <span className="muted" style={{ fontWeight: 400 }}>: {msg}</span>}
        </h1>
        {d.culprit && <p className="mono muted" style={{ fontSize: "var(--text-sm)" }}>{d.culprit}</p>}
        <div className="toolbar">
          {d.status !== "resolved" && (
            <button className="btn primary" onClick={() => setStatus("resolved")}>
              Resolve <kbd>r</kbd>
            </button>
          )}
          {d.status !== "ignored" && (
            <button className="btn" onClick={() => setStatus("ignored")}>
              Ignore <kbd>i</kbd>
            </button>
          )}
          {d.status !== "unresolved" && (
            <button className="btn" onClick={() => setStatus("unresolved")}>
              Unresolve <kbd>u</kbd>
            </button>
          )}
        </div>
      </header>

      <div className="stat-grid">
        <div className="stat">
          <span className="label">Events</span>
          <span className="value" title={String(d.event_count)}>{compact(d.event_count)}</span>
        </div>
        <div className="stat">
          <span className="label">Users</span>
          <span className="value" title={String(d.user_count)}>{compact(d.user_count)}</span>
        </div>
        <div className="stat">
          <span className="label">First seen</span>
          <span className="value" title={exact(d.first_seen)}>{relative(d.first_seen)}</span>
        </div>
        <div className="stat">
          <span className="label">Last seen</span>
          <span className="value" title={exact(d.last_seen)}>{relative(d.last_seen)}</span>
        </div>
        <div className="stat">
          <span className="label">Last 24 hours</span>
          <Sparkline values={d.trend} />
        </div>
      </div>

      <div className="issue-layout">
        <section className="panel" style={{ overflow: "hidden" }} aria-label="Event">
          <div className="page-head" style={{ padding: "var(--space-2) var(--space-3)", borderBottom: "1px solid var(--line)" }}>
            <span className="muted" style={{ fontSize: "var(--text-sm)" }}>
              {event ? (
                <>
                  Event {index + 1}
                  {more ? "" : ` of ${events.length}`} · <span title={exact(event.timestamp)}>{relative(event.timestamp)}</span>
                  {event.release && <> · {event.release}</>}
                </>
              ) : more ? (
                "Loading events…"
              ) : (
                "No events stored for this issue."
              )}
            </span>
            <div className="toolbar">
              <button className="btn small" onClick={newer} disabled={index === 0} aria-label="Newer event">
                ← Newer <kbd>[</kbd>
              </button>
              <button className="btn small" onClick={() => void older()} disabled={index >= events.length - 1 && !more} aria-label="Older event">
                Older <kbd>]</kbd> →
              </button>
            </div>
          </div>

          {event && sym && sym.status !== "symbolicated" && (
            // "missing" only means no debug files exist for this release: the
            // code may never have been minified, so it's a calm note, not a
            // warning. "partial" and "failed" mean something did go wrong.
            <div className={`banner${sym.status === "missing" ? "" : " warn"}`} role="note" style={{ margin: "var(--space-3)", display: "grid", gap: 4 }}>
              <strong>
                {sym.status === "missing"
                  ? `No ${d.platform === "dart" ? "Dart symbols" : "source maps"} for ${event.release ?? "this build"}`
                  : sym.status === "partial"
                    ? "Some frames couldn't be mapped to your source"
                    : "Symbolication failed for this event"}
              </strong>
              <span>
                {sym.status === "missing" ? "If the frames below look minified or obfuscated, upload " : "Check the notes below, then upload "}
                {d.platform === "dart" ? "Dart symbols" : "source maps"} for this release to see your original code.{" "}
                <Link className="link" to={`/p/${projectId}/settings/files`}>
                  How to upload
                </Link>
              </span>
              {sym.notes?.map((n) => (
                <span key={n} className="mono faint" style={{ fontSize: "var(--text-xs)" }}>
                  {n}
                </span>
              ))}
            </div>
          )}

          <div className="tabs" role="tablist" aria-label="Event details" style={{ padding: "0 var(--space-3)" }}>
            {tabs.map((t, i) => (
              <button key={t.id} role="tab" aria-selected={tab === t.id} onClick={() => setTab(t.id)} title={`Shortcut: ${i + 1}`}>
                {t.label}
                {t.id === "crumbs" && event?.breadcrumbs?.length ? <span className="count">{event.breadcrumbs.length}</span> : null}
              </button>
            ))}
            {tab === "stack" && hasRaw && (
              <label className="switch" style={{ marginLeft: "auto", fontSize: "var(--text-xs)" }}>
                <input type="checkbox" checked={showRaw} onChange={(e) => setShowRaw(e.target.checked)} />
                Show original
              </label>
            )}
          </div>

          {event && (
            <div role="tabpanel">
              {tab === "stack" && (
                <>
                  <p style={{ padding: "var(--space-3)", margin: 0, borderBottom: "1px solid var(--line)" }}>
                    <strong>{event.exception.type}</strong>
                    {event.exception.message && <span className="muted">: {event.exception.message}</span>}
                  </p>
                  <StackTrace frames={frames} showRaw={showRaw} />
                </>
              )}
              {tab === "crumbs" && <Breadcrumbs crumbs={event.breadcrumbs ?? []} error={`${event.exception.type}: ${event.exception.message}`} timestamp={event.timestamp} />}
              {tab === "context" && <Context event={event} />}
              {tab === "json" && (
                <div style={{ padding: "var(--space-3)" }}>
                  <Code>{JSON.stringify(event, null, 2)}</Code>
                </div>
              )}
            </div>
          )}
        </section>

        <aside style={{ display: "grid", gap: "var(--space-4)" }}>
          <section className="panel activity" aria-labelledby="activity-title">
            <h3 id="activity-title">Activity</h3>
            <ul>
              {d.activity.map((a, i) => (
                <li key={i}>
                  <span className={`mark ${a.kind}`} aria-hidden="true" />
                  <span>
                    {activityText[a.kind] ?? a.kind}
                    {a.release && <span className="muted"> in {a.release}</span>}
                    {a.user && <span className="muted"> by {a.user}</span>}
                    <br />
                    <span className="faint" title={exact(a.created_at)}>
                      {relative(a.created_at)}
                    </span>
                  </span>
                </li>
              ))}
            </ul>
          </section>
          {(d.releases.length > 0 || d.environments.length > 0) && (
            <section className="panel activity">
              {d.releases.length > 0 && (
                <div style={{ display: "grid", gap: 6 }}>
                  <h3>Releases</h3>
                  <div className="issue-meta">
                    {d.releases.map((r) => (
                      <span key={r} className="chip">
                        {r}
                      </span>
                    ))}
                  </div>
                </div>
              )}
              {d.environments.length > 0 && (
                <div style={{ display: "grid", gap: 6 }}>
                  <h3>Environments</h3>
                  <div className="issue-meta">
                    {d.environments.map((e) => (
                      <span key={e} className="chip">
                        {e}
                      </span>
                    ))}
                  </div>
                </div>
              )}
            </section>
          )}
          <p className="faint mono" style={{ fontSize: "var(--text-xs)", overflowWrap: "anywhere" }} title="Events with this fingerprint are grouped into this issue">
            fingerprint {d.fingerprint.slice(0, 16)}
          </p>
        </aside>
      </div>
    </div>
  );
}
