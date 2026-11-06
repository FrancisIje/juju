import { useRef } from "react";
import type { AdminOrg, AdminOrgDetail, AdminOverview, AdminProject, AdminStatus } from "../lib/api";
import { useApp } from "../lib/app";
import { useShortcuts } from "../lib/keys";
import { percent } from "../lib/perf";
import { Link, navigate, useLocation } from "../lib/router";
import { compact, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";

// The super admin view: every organization and project on this server, for
// the people who run it. Only counts and names, on purpose. To see a
// project's issues you have to be in its organization, like anyone else.

export type AdminTab = "organizations" | "projects";

const statuses: Record<AdminStatus, { label: string; tone: string; why: string }> = {
  active: { label: "Active", tone: "good", why: "Received events in the last 7 days." },
  quiet: { label: "Quiet", tone: "ignored", why: "Has sent events before, but none in the last 7 days." },
  no_events: { label: "No events yet", tone: "", why: "Has never sent an event." },
  near_quota: { label: "Near quota", tone: "warn", why: "Has used at least 80% of its monthly quota." },
  over_quota: { label: "Over quota", tone: "bad", why: "Has used its monthly quota, so new events are being refused." },
  no_projects: { label: "No projects", tone: "", why: "This organization hasn't created a project." },
};

const projectStatuses: AdminStatus[] = ["over_quota", "near_quota", "active", "quiet", "no_events"];
const isAdminStatus = (s: string | null): s is AdminStatus => s !== null && s in statuses;

function AdminChip({ status }: { status: AdminStatus }) {
  const s = statuses[status];
  return (
    <span className={`chip ${s.tone}`} title={s.why}>
      <span className="dot" />
      {s.label}
    </span>
  );
}

function LastEvent({ at }: { at: string | null }) {
  if (!at) return <span className="faint">Never</span>;
  return <span title={exact(at)}>{relative(at)}</span>;
}

/** "1.2k / 10k" and a meter for projects with a quota; just the count otherwise. */
function Usage({ p }: { p: AdminProject }) {
  if (p.monthly_quota === null) return <span>{compact(p.events_this_month)}</span>;
  const used = p.monthly_quota > 0 ? Math.min(1, p.events_this_month / p.monthly_quota) : 1;
  return (
    <div style={{ display: "grid", gap: 4, justifyItems: "end" }}>
      <span>
        {compact(p.events_this_month)} / {compact(p.monthly_quota)}
      </span>
      <div className="meter" style={{ width: "100%" }} aria-hidden="true">
        <span style={{ width: `${used * 100}%` }} />
      </div>
    </div>
  );
}

function CrashFree({ p }: { p: AdminProject }) {
  if (p.sessions_24h === 0) return <span className="faint">–</span>;
  return <span title={`${p.crashed_24h} of ${p.sessions_24h} sessions crashed`}>{percent(1 - p.crashed_24h / p.sessions_24h)}</span>;
}

/** A project name, linked only if you're in its organization: anyone else's would be "not found". */
function ProjectName({ p }: { p: AdminProject }) {
  const { me } = useApp();
  const member = me.organizations.some((o) => o.id === p.organization_id);
  return member ? <Link className="link" to={`/p/${p.id}/issues`}>{p.name}</Link> : <span>{p.name}</span>;
}

function Failed({ message, retry }: { message: string; retry: () => void }) {
  return (
    <div className="banner warn" role="alert">
      {message}{" "}
      <button className="btn small" onClick={retry}>
        Retry
      </button>
    </div>
  );
}

function matches(q: string, ...fields: string[]) {
  const words = q.toLowerCase().split(/\s+/).filter(Boolean);
  const text = fields.join(" ").toLowerCase();
  return words.every((w) => text.includes(w));
}

// Refused events come first, then whoever sends the most.
const urgency = (s: AdminStatus) => (s === "over_quota" ? 0 : s === "near_quota" ? 1 : 2);
const byUrgency = (a: AdminProject, b: AdminProject) => urgency(a.status) - urgency(b.status) || b.events_this_month - a.events_this_month || a.name.localeCompare(b.name);
const byEvents = (a: AdminOrg, b: AdminOrg) => b.events_this_month - a.events_this_month || a.name.localeCompare(b.name);

export function Admin({ tab }: { tab: AdminTab }) {
  const { path, query } = useLocation();
  // Numbers change slowly and this query reads every project, so once a
  // minute is plenty.
  const { data, error, loading, reload } = useApi<AdminOverview>("/api/ui/admin/overview", 60_000);
  const search = useRef<HTMLInputElement>(null);
  useShortcuts({ "/": () => search.current?.focus() });

  const q = query.get("q") ?? "";
  const status = query.get("status");
  function set(key: string, value: string) {
    const next = new URLSearchParams(query);
    if (value) next.set(key, value);
    else next.delete(key);
    navigate(`${path}${next.size ? `?${next}` : ""}`, { replace: true });
  }

  const t = data?.totals;
  const attention = t ? (t.by_status.over_quota ?? 0) + (t.by_status.near_quota ?? 0) : 0;
  const orgs = (data?.organizations ?? []).filter((o) => matches(q, o.name, o.owner_email)).sort(byEvents);
  const projects = (data?.projects ?? [])
    .filter((p) => matches(q, p.name, p.organization) && (!isAdminStatus(status) || p.status === status))
    .sort(byUrgency);

  return (
    <div className="page">
      <div className="page-head">
        <div>
          <h1>Super admin</h1>
          <p className="muted" style={{ margin: 0 }}>
            Every organization and project on this server. Counts only: issue details stay private to each organization.
          </p>
        </div>
      </div>

      {error && <Failed message={error.message} retry={() => void reload()} />}

      {t && (
        <div className="stat-grid" aria-busy={loading}>
          <div className="stat">
            <span className="label">Organizations</span>
            <span className="value">{compact(t.organizations)}</span>
            <span className="sub">{t.new_organizations_7d} new in 7 days</span>
          </div>
          <div className="stat">
            <span className="label">Projects</span>
            <span className="value">{compact(t.projects)}</span>
            <span className="sub">{t.new_projects_7d} new in 7 days</span>
          </div>
          <div className="stat">
            <span className="label">People</span>
            <span className="value">{compact(t.users)}</span>
            <span className="sub">{t.new_users_7d} new in 7 days</span>
          </div>
          <div className="stat">
            <span className="label">Events this month</span>
            <span className="value">{compact(t.events_this_month)}</span>
            <span className="sub">{compact(t.events_last_month)} last month</span>
          </div>
          <div className="stat">
            <span className="label">Errors, 24 hours</span>
            <span className="value">{compact(t.errors_24h)}</span>
            <span className="sub">{compact(t.unresolved)} unresolved issues</span>
          </div>
          <div className="stat">
            <span className="label">Near or over quota</span>
            <span className="value">{attention}</span>
            <span className="sub">{t.by_status.quiet ?? 0} quiet projects</span>
          </div>
        </div>
      )}

      <div className="tabs" role="tablist" aria-label="Super admin views">
        {(["organizations", "projects"] as const).map((name) => (
          <Link key={name} role="tab" to={`/admin/${name}${q ? `?q=${encodeURIComponent(q)}` : ""}`} aria-selected={tab === name}>
            {name === "organizations" ? "Organizations" : "Projects"}
            {t && <span className="count">{name === "organizations" ? t.organizations : t.projects}</span>}
          </Link>
        ))}
      </div>

      <div className="toolbar">
        <input
          ref={search}
          className="input"
          style={{ flex: "1 1 220px", maxWidth: 360 }}
          aria-label={tab === "projects" ? "Search projects" : "Search organizations"}
          placeholder={tab === "projects" ? "Search by project or organization…  /" : "Search by name or owner email…  /"}
          value={q}
          onChange={(e) => set("q", e.target.value)}
        />
        {tab === "projects" && (
          <select className="select" style={{ width: "auto" }} aria-label="Filter by status" value={isAdminStatus(status) ? status : ""} onChange={(e) => set("status", e.target.value)}>
            <option value="">All statuses</option>
            {projectStatuses.map((s) => (
              <option key={s} value={s}>
                {statuses[s].label} ({t?.by_status[s] ?? 0})
              </option>
            ))}
          </select>
        )}
      </div>

      {data && tab === "organizations" && (
        <section className="panel" style={{ overflowX: "auto" }}>
          <table className="table admin-table">
            <thead>
              <tr>
                <th>Organization</th>
                <th>Status</th>
                <th className="hide-narrow">Owner</th>
                <th className="num">Projects</th>
                <th className="num hide-narrow">People</th>
                <th className="num">Events this month</th>
                <th className="num hide-narrow">Errors, 24 h</th>
                <th className="hide-narrow">Last event</th>
                <th className="hide-narrow">Created</th>
              </tr>
            </thead>
            <tbody>
              {orgs.map((o) => (
                <tr key={o.id}>
                  <td>
                    <Link className="link" to={`/admin/orgs/${o.id}`}>{o.name}</Link>
                  </td>
                  <td className="nowrap">
                    <AdminChip status={o.status} />
                  </td>
                  <td className="hide-narrow">{o.owner_email || <span className="faint">No owner</span>}</td>
                  <td className="num">{o.projects}</td>
                  <td className="num hide-narrow">{o.members}</td>
                  <td className="num">{compact(o.events_this_month)}</td>
                  <td className="num hide-narrow">{compact(o.errors_24h)}</td>
                  <td className="hide-narrow nowrap">
                    <LastEvent at={o.last_event_at} />
                  </td>
                  <td className="hide-narrow nowrap faint" title={exact(o.created_at)}>
                    {relative(o.created_at)}
                  </td>
                </tr>
              ))}
              {orgs.length === 0 && (
                <tr>
                  <td colSpan={9} className="muted">
                    {q ? `No organization matches “${q}”.` : "No organizations yet. They appear here when someone signs up."}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </section>
      )}

      {data && tab === "projects" && (
        <section className="panel" style={{ overflowX: "auto" }}>
          <table className="table admin-table">
            <thead>
              <tr>
                <th>Project</th>
                <th className="hide-narrow">Organization</th>
                <th>Status</th>
                <th className="num">Events this month</th>
                <th className="num hide-narrow">Errors, 24 h</th>
                <th className="num hide-narrow">Crash‑free, 24 h</th>
                <th className="num hide-narrow">Unresolved</th>
                <th className="hide-narrow">Last event</th>
              </tr>
            </thead>
            <tbody>
              {projects.map((p) => (
                <tr key={p.id}>
                  <td>
                    <ProjectName p={p} />
                  </td>
                  <td className="hide-narrow">
                    <Link className="link" to={`/admin/orgs/${p.organization_id}`}>{p.organization}</Link>
                  </td>
                  <td className="nowrap">
                    <AdminChip status={p.status} />
                  </td>
                  <td className="num">
                    <Usage p={p} />
                  </td>
                  <td className="num hide-narrow">{compact(p.errors_24h)}</td>
                  <td className="num hide-narrow">
                    <CrashFree p={p} />
                  </td>
                  <td className="num hide-narrow">{compact(p.unresolved)}</td>
                  <td className="hide-narrow nowrap">
                    <LastEvent at={p.last_event_at} />
                  </td>
                </tr>
              ))}
              {projects.length === 0 && (
                <tr>
                  <td colSpan={8} className="muted">
                    {q || isAdminStatus(status) ? "No project matches these filters." : "No projects yet."}
                  </td>
                </tr>
              )}
            </tbody>
          </table>
        </section>
      )}
    </div>
  );
}

export function AdminOrganization({ orgId }: { orgId: string }) {
  const { data, error, reload } = useApi<AdminOrgDetail>(`/api/ui/admin/orgs/${encodeURIComponent(orgId)}`, 60_000);
  const back = (
    <Link to="/admin/organizations" className="muted">
      ← All organizations
    </Link>
  );
  if (error?.status === 404) {
    return (
      <div className="page">
        {back}
        <div className="empty">
          <h1>Organization not found</h1>
          <p className="muted">There's no organization with this id on the server.</p>
        </div>
      </div>
    );
  }
  const o = data?.organization;
  return (
    <div className="page">
      {back}
      {error && <Failed message={error.message} retry={() => void reload()} />}
      {o && data && (
        <>
          <div className="page-head">
            <h1>{o.name}</h1>
            <AdminChip status={o.status} />
          </div>
          <div className="stat-grid">
            <div className="stat">
              <span className="label">Events this month</span>
              <span className="value">{compact(o.events_this_month)}</span>
            </div>
            <div className="stat">
              <span className="label">Errors, 24 hours</span>
              <span className="value">{compact(o.errors_24h)}</span>
            </div>
            <div className="stat">
              <span className="label">Unresolved issues</span>
              <span className="value">{compact(o.unresolved)}</span>
            </div>
            <div className="stat">
              <span className="label">Created</span>
              <span className="value" title={exact(o.created_at)}>
                {relative(o.created_at)}
              </span>
            </div>
          </div>

          <h2 style={{ margin: 0, fontSize: "var(--text-md)" }}>Projects</h2>
          <section className="panel" style={{ overflowX: "auto" }}>
            <table className="table admin-table">
              <thead>
                <tr>
                  <th>Project</th>
                  <th>Status</th>
                  <th className="num">Events this month</th>
                  <th className="num hide-narrow">Errors, 24 h</th>
                  <th className="num hide-narrow">Crash‑free, 24 h</th>
                  <th className="hide-narrow">Last event</th>
                </tr>
              </thead>
              <tbody>
                {[...data.projects].sort(byUrgency).map((p) => (
                  <tr key={p.id}>
                    <td>
                      <ProjectName p={p} />
                    </td>
                    <td className="nowrap">
                      <AdminChip status={p.status} />
                    </td>
                    <td className="num">
                      <Usage p={p} />
                    </td>
                    <td className="num hide-narrow">{compact(p.errors_24h)}</td>
                    <td className="num hide-narrow">
                      <CrashFree p={p} />
                    </td>
                    <td className="hide-narrow nowrap">
                      <LastEvent at={p.last_event_at} />
                    </td>
                  </tr>
                ))}
                {data.projects.length === 0 && (
                  <tr>
                    <td colSpan={6} className="muted">
                      This organization hasn't created a project.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </section>

          <h2 style={{ margin: 0, fontSize: "var(--text-md)" }}>People</h2>
          <section className="panel" style={{ overflowX: "auto" }}>
            <table className="table admin-table">
              <thead>
                <tr>
                  <th>Email</th>
                  <th className="hide-narrow">Name</th>
                  <th>Role</th>
                  <th className="hide-narrow">Joined</th>
                </tr>
              </thead>
              <tbody>
                {data.members.map((m) => (
                  <tr key={m.user_id}>
                    <td>{m.email}</td>
                    <td className="hide-narrow">{m.name || <span className="faint">–</span>}</td>
                    <td>{m.role === "owner" ? "Owner" : "Member"}</td>
                    <td className="hide-narrow nowrap faint" title={exact(m.joined_at)}>
                      {relative(m.joined_at)}
                    </td>
                  </tr>
                ))}
                {data.members.length === 0 && (
                  <tr>
                    <td colSpan={4} className="muted">
                      Nobody belongs to this organization.
                    </td>
                  </tr>
                )}
              </tbody>
            </table>
          </section>
        </>
      )}
    </div>
  );
}
