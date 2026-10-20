import { useEffect, useMemo, useRef, useState } from "react";
import { Pixels } from "../components/Pixels";
import { Sparkline } from "../components/Sparkline";
import { PlatformChip, StatusChip } from "../components/StatusChip";
import { api, ApiError, type Counts, type IssueRow, type Status } from "../lib/api";
import { useApp } from "../lib/app";
import { splitTitle } from "../lib/frames";
import { useShortcuts } from "../lib/keys";
import { navigate, useLocation } from "../lib/router";
import { compact, exact, relative } from "../lib/time";
import { useLive } from "../lib/live";
import { useApi } from "../lib/useApi";
import { Onboarding } from "./Onboarding";

const tabs: { id: Status | "all"; label: string }[] = [
  { id: "unresolved", label: "Unresolved" },
  { id: "resolved", label: "Resolved" },
  { id: "ignored", label: "Ignored" },
  { id: "all", label: "All" },
];

const sorts = [
  { id: "last_seen", label: "Last seen" },
  { id: "first_seen", label: "Newest" },
  { id: "events", label: "Most events" },
  { id: "users", label: "Most users" },
];

const PAGE = 50;

interface ListResponse {
  issues: IssueRow[];
  total: number;
  counts: Counts;
}

export function Issues({ projectId }: { projectId: number }) {
  const { toast, reloadMe } = useApp();
  const { query } = useLocation();
  // Filters live in the URL, so a filtered list can be bookmarked or shared.
  const status = (query.get("status") ?? "unresolved") as Status | "all";
  const q = query.get("q") ?? "";
  const env = query.get("environment") ?? "";
  const sort = query.get("sort") ?? "last_seen";
  const [limit, setLimit] = useState(PAGE);
  const [search, setSearch] = useState(q);
  const [focus, setFocus] = useState(0);
  const [selected, setSelected] = useState<Set<number>>(new Set());
  const searchRef = useRef<HTMLInputElement>(null);

  const params = new URLSearchParams({ status, sort, limit: String(limit) });
  if (q) params.set("q", q);
  if (env) params.set("environment", env);
  const { data, error, loading, reload, setData } = useApi<ListResponse>(`/api/ui/projects/${projectId}/issues?${params}`);
  const project = useApi<{ environments: string[]; counts: Counts }>(`/api/ui/projects/${projectId}`);

  // New errors and teammates' changes appear without a reload.
  useLive(projectId, () => {
    void reload(true);
    void project.reload(true);
    void reloadMe();
  });

  const issues = useMemo(() => data?.issues ?? [], [data]);
  useEffect(() => setSelected(new Set()), [status, q, env, sort, projectId]);
  useEffect(() => setFocus((f) => Math.min(f, Math.max(0, issues.length - 1))), [issues.length]);
  useEffect(() => setSearch(q), [q]);

  function setParam(key: string, value: string) {
    const next = new URLSearchParams(query);
    if (value) next.set(key, value);
    else next.delete(key);
    setLimit(PAGE);
    navigate(`/p/${projectId}/issues${next.size ? `?${next}` : ""}`, { replace: key === "q" });
  }

  // Search as you type, but wait a moment so each keystroke isn't a request.
  useEffect(() => {
    if (search === q) return;
    const t = setTimeout(() => setParam("q", search.trim()), 250);
    return () => clearTimeout(t);
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [search]);

  async function setStatus(ids: number[], next: Status) {
    if (ids.length === 0) return;
    // Update the list right away (optimistic), then confirm with the server.
    const before = data;
    if (data) setData({ ...data, issues: data.issues.filter((i) => !ids.includes(i.id) || status === "all" || next === status) });
    try {
      const r = await api<{ changed: number }>(`/api/ui/projects/${projectId}/issues/status`, { method: "POST", body: { ids, status: next } });
      const verb = next === "resolved" ? "Resolved" : next === "ignored" ? "Ignored" : "Marked unresolved";
      toast(`${verb} ${r.changed} issue${r.changed === 1 ? "" : "s"}`);
      setSelected(new Set());
      void reload(true);
      void reloadMe();
    } catch (err) {
      if (before) setData(before);
      toast(err instanceof ApiError ? err.message : "Couldn't update the issue.");
    }
  }

  const targets = () => (selected.size ? [...selected] : issues[focus] ? [issues[focus]!.id] : []);
  useShortcuts({
    j: () => setFocus((f) => Math.min(issues.length - 1, f + 1)),
    k: () => setFocus((f) => Math.max(0, f - 1)),
    ArrowDown: () => setFocus((f) => Math.min(issues.length - 1, f + 1)),
    ArrowUp: () => setFocus((f) => Math.max(0, f - 1)),
    Enter: () => issues[focus] && navigate(`/p/${projectId}/issues/${issues[focus]!.id}`),
    o: () => issues[focus] && navigate(`/p/${projectId}/issues/${issues[focus]!.id}`),
    x: () => issues[focus] && toggle(issues[focus]!.id),
    r: () => void setStatus(targets(), "resolved"),
    i: () => void setStatus(targets(), "ignored"),
    u: () => void setStatus(targets(), "unresolved"),
    "/": () => searchRef.current?.focus(),
  });

  // Keep the focused row in view as j/k move through a long list.
  useEffect(() => {
    document.querySelector(`[data-row="${focus}"]`)?.scrollIntoView({ block: "nearest" });
  }, [focus]);

  function toggle(id: number) {
    setSelected((s) => {
      const n = new Set(s);
      if (n.has(id)) n.delete(id);
      else n.add(id);
      return n;
    });
  }

  const counts = data?.counts ?? project.data?.counts;
  const neverReceived = counts && counts.unresolved + counts.resolved + counts.ignored === 0 && !q && !env;
  const environments = project.data?.environments ?? [];

  return (
    <div className="page">
      <div className="page-head">
        <h1>Issues</h1>
        <div className="toolbar">
          <label className="sr-only" htmlFor="issue-search">
            Search issues
          </label>
          <input
            id="issue-search"
            ref={searchRef}
            className="input"
            style={{ width: 240 }}
            placeholder="Search title or location   /"
            value={search}
            onChange={(e) => setSearch(e.target.value)}
            onKeyDown={(e) => e.key === "Escape" && (e.currentTarget.blur(), setSearch(""))}
          />
          {environments.length > 1 && (
            <select className="select" style={{ width: "auto" }} aria-label="Environment" value={env} onChange={(e) => setParam("environment", e.target.value)}>
              <option value="">All environments</option>
              {environments.map((e) => (
                <option key={e}>{e}</option>
              ))}
            </select>
          )}
          <select className="select" style={{ width: "auto" }} aria-label="Sort by" value={sort} onChange={(e) => setParam("sort", e.target.value === "last_seen" ? "" : e.target.value)}>
            {sorts.map((s) => (
              <option key={s.id} value={s.id}>
                {s.label}
              </option>
            ))}
          </select>
        </div>
      </div>

      {neverReceived ? (
        <Onboarding projectId={projectId} />
      ) : (
        <>
          <div className="tabs" role="tablist" aria-label="Status">
            {tabs.map((t) => (
              <button key={t.id} role="tab" aria-selected={status === t.id} onClick={() => setParam("status", t.id === "unresolved" ? "" : t.id)}>
                {t.label}
                {counts && t.id !== "all" && <span className="count">{compact(counts[t.id])}</span>}
              </button>
            ))}
          </div>

          {error && (
            <div className="banner warn" role="alert">
              {error.message}{" "}
              <button className="btn small" onClick={() => reload()}>
                Retry
              </button>
            </div>
          )}

          <div className="panel" style={{ overflow: "hidden" }}>
            <div className="col-head" aria-hidden="true">
              <span />
              <span>Issue</span>
              <span className="hide-narrow">Last 24 hours</span>
              <span className="num">Events</span>
              <span className="num hide-narrow">Users</span>
              <span className="when">Last seen</span>
            </div>
            <div className="issue-list" role="list" aria-busy={loading}>
              {issues.map((issue, i) => {
                const [type, msg] = splitTitle(issue.title);
                const regressed = issue.regressed;
                return (
                  <div
                    key={issue.id}
                    role="listitem"
                    className="issue-row"
                    data-row={i}
                    data-focused={i === focus}
                    data-selected={selected.has(issue.id)}
                    onClick={(e) => {
                      if ((e.target as HTMLElement).closest("input")) return;
                      if (e.metaKey || e.ctrlKey) window.open(`/p/${projectId}/issues/${issue.id}`, "_blank");
                      else navigate(`/p/${projectId}/issues/${issue.id}`);
                    }}
                    onMouseEnter={() => setFocus(i)}
                  >
                    <input type="checkbox" aria-label={`Select ${issue.title}`} checked={selected.has(issue.id)} onChange={() => toggle(issue.id)} />
                    <div style={{ minWidth: 0 }}>
                      <a className="issue-title" href={`/p/${projectId}/issues/${issue.id}`} onClick={(e) => e.preventDefault()}>
                        <span className="type">{type}</span>
                        <span className="msg">{msg}</span>
                      </a>
                      <div className="issue-meta">
                        {status === "all" || regressed ? <StatusChip status={issue.status} regressed={regressed} /> : null}
                        {issue.culprit && <span className="culprit">{issue.culprit}</span>}
                        <PlatformChip platform={issue.platform} />
                        {issue.last_release && <span>{issue.last_release}</span>}
                        <span title={exact(issue.first_seen)}>first {relative(issue.first_seen)}</span>
                      </div>
                    </div>
                    <span className="hide-narrow">
                      <Sparkline values={issue.trend} />
                    </span>
                    <span className="num" title={`${issue.event_count} events`}>
                      {compact(issue.event_count)}
                    </span>
                    <span className="num hide-narrow" title={`${issue.user_count} affected users`}>
                      {compact(issue.user_count)}
                    </span>
                    <span className="when" title={exact(issue.last_seen)}>
                      {relative(issue.last_seen)}
                    </span>
                  </div>
                );
              })}
            </div>
            {!loading && issues.length === 0 && !error && (
              <div className="empty">
                <Pixels className="" seed={status.length + q.length} density={0.3} cell={12} />
                <h2>{q || env ? "No issues match" : status === "unresolved" ? "Nothing unresolved" : `No ${status} issues`}</h2>
                <p className="muted">{q || env ? "Try a different search or environment." : status === "unresolved" ? "New errors show up here within seconds of happening." : "Issues you mark this way are listed here."}</p>
              </div>
            )}
          </div>

          {data && issues.length < data.total && (
            <button className="btn" style={{ justifySelf: "center" }} onClick={() => setLimit((l) => l + PAGE)}>
              Show more ({compact(data.total - issues.length)} left)
            </button>
          )}

          {selected.size > 0 && (
            <div className="bulk-bar" role="toolbar" aria-label="Bulk actions">
              <span className="muted" style={{ padding: "0 var(--space-2)" }}>
                {selected.size} selected
              </span>
              <button className="btn small" onClick={() => setStatus([...selected], "resolved")}>
                Resolve <kbd>r</kbd>
              </button>
              <button className="btn small" onClick={() => setStatus([...selected], "ignored")}>
                Ignore <kbd>i</kbd>
              </button>
              <button className="btn small" onClick={() => setStatus([...selected], "unresolved")}>
                Unresolve <kbd>u</kbd>
              </button>
              <button className="btn ghost small" onClick={() => setSelected(new Set())}>
                Clear
              </button>
            </div>
          )}
          <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
            <kbd>j</kbd> <kbd>k</kbd> move · <kbd>x</kbd> select · <kbd>r</kbd> resolve · <kbd>i</kbd> ignore · <kbd>?</kbd> all shortcuts
          </p>
        </>
      )}
    </div>
  );
}
