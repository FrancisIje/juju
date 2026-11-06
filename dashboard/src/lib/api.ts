// The dashboard's only way to talk to the server. Every request:
//  - is same-origin, so the session cookie goes along automatically;
//  - carries the CSRF header the server requires for changes (a malicious
//    site can't add custom headers to cross-site requests);
//  - turns error responses into ApiError with the server's message, which
//    is written for people and safe to show.

export const CSRF_HEADER = "X-Juju-CSRF";

export class ApiError extends Error {
  constructor(
    message: string,
    readonly status: number,
  ) {
    super(message);
  }
}

let onUnauthorized: () => void = () => {};
/** Called when the session has ended, to send people to the login page. */
export function setUnauthorizedHandler(fn: () => void) {
  onUnauthorized = fn;
}

export async function api<T>(path: string, init: { method?: string; body?: unknown; signal?: AbortSignal } = {}): Promise<T> {
  const method = init.method ?? "GET";
  let res: Response;
  try {
    res = await fetch(path, {
      method,
      credentials: "same-origin",
      headers: { "Content-Type": "application/json", [CSRF_HEADER]: "1" },
      body: init.body === undefined ? undefined : JSON.stringify(init.body),
      signal: init.signal,
    });
  } catch (err) {
    if ((err as Error).name === "AbortError") throw err;
    throw new ApiError("Can't reach the juju server. Check your connection and try again.", 0);
  }
  if (res.status === 204) return undefined as T;
  const body = await res.json().catch(() => ({}));
  if (!res.ok) {
    if (res.status === 401 && !path.startsWith("/api/ui/auth/")) onUnauthorized();
    const msg = (body as { error?: string }).error;
    // The server writes messages in lowercase, like Go errors; on screen they
    // start a sentence.
    throw new ApiError(msg ? msg.charAt(0).toUpperCase() + msg.slice(1) : `The server answered ${res.status}.`, res.status);
  }
  return body as T;
}

// Shapes returned by /api/ui. They mirror server/internal/store/dashboard.go.

export interface User {
  id: number;
  email: string;
  name: string;
  /** Only sent when true. The server checks this itself on every admin request. */
  is_superadmin?: boolean;
}

export interface ProjectSummary {
  id: number;
  name: string;
  unresolved: number;
}

export interface Org {
  id: number;
  name: string;
  role: string;
  projects: ProjectSummary[];
}

export interface Me {
  user: User;
  organizations: Org[];
}

export interface ProjectInfo {
  id: number;
  name: string;
  organization: string;
  public_key: string;
  dsn: string;
  secret_key?: string;
}

export type Status = "unresolved" | "resolved" | "ignored";

export interface Counts {
  unresolved: number;
  resolved: number;
  ignored: number;
}

export interface IssueRow {
  id: number;
  title: string;
  culprit: string;
  status: Status;
  platform: string;
  last_release: string;
  event_count: number;
  user_count: number;
  first_seen: string;
  last_seen: string;
  status_changed_at: string | null;
  /** Unresolved because it happened again after being resolved. */
  regressed: boolean;
  trend: number[];
}

export interface Activity {
  kind: "created" | "resolved" | "ignored" | "reopened" | "regressed";
  user?: string;
  release?: string;
  created_at: string;
}

/** The git commit a release was built from, recorded by `juju release` in CI. */
export interface ReleaseCommit {
  name: string;
  commit: string;
  repository: string;
  updated_at: string;
}

/** A link attached to an issue, usually the pull request fixing it. */
export interface IssueLink {
  url: string;
  title: string;
  created_at: string;
}

export interface IssueDetail extends IssueRow {
  fingerprint: string;
  environments: string[];
  releases: string[];
  commits: ReleaseCommit[];
  links: IssueLink[];
  activity: Activity[];
}

export interface Frame {
  function?: string;
  filename?: string;
  lineno?: number;
  colno?: number;
  in_app: boolean;
  raw?: { function?: string; filename?: string; lineno?: number; colno?: number };
}

export interface Breadcrumb {
  type: string;
  message?: string;
  timestamp?: string;
  data?: Record<string, unknown>;
}

export interface EventPayload {
  event_id: string;
  timestamp: string;
  received_at: string;
  platform: string;
  release?: string;
  environment: string;
  exception: { type: string; message: string; stacktrace?: { frames: Frame[] } };
  breadcrumbs?: Breadcrumb[];
  context?: Record<string, unknown>;
  user?: Record<string, string>;
  tags?: Record<string, string>;
  sdk: { name: string; version: string };
  repairs?: string[];
  symbolication?: { status: string; notes?: string[] };
}

export interface AlertsView {
  slack_webhook_set: boolean;
  slack_webhook_hint: string;
  email_to: string[];
  on_new_issue: boolean;
  on_regression: boolean;
  email_available: boolean;
  webhook_url: string;
  /** Only in the response that created it: shown once, like the secret key. */
  webhook_secret?: string;
  deliveries: { id: number; issue_id: number | null; kind: string; channel: string; ok: boolean; error: string; created_at: string }[];
}

export interface DebugFile {
  id: number;
  kind: string;
  release?: string;
  name: string;
  size: number;
  created_at: string;
}

export interface Member {
  user_id: number;
  email: string;
  name: string;
  role: "owner" | "member";
  joined_at: string;
}

export interface PendingInvite {
  id: number;
  created_by: string;
  created_at: string;
  expires_at: string;
}

export interface TeamView {
  members: Member[];
  /** Only sent to owners. */
  invites?: PendingInvite[];
  your_role: "owner" | "member";
  you: number;
}

export interface InviteInfo {
  organization_id: number;
  organization: string;
  invited_by: string;
  expires_at: string;
}

// Phase 8: release health and performance. Mirror server/internal/store/performance.go.

export type Period = "24h" | "7d" | "14d" | "30d";

export interface SessionStats {
  sessions: number;
  crashed: number;
  errored: number;
  users: number;
  crashed_users: number;
  /** 0 to 1; null when there were no sessions (unknown is not 100 %). */
  crash_free_sessions: number | null;
  crash_free_users: number | null;
}

export interface ReleaseRow extends SessionStats {
  release: string;
  /** This release's share of all sessions in the period, 0 to 1. */
  adoption: number;
  first_seen: string;
  last_seen: string;
}

export interface SessionPoint {
  time: string;
  sessions: number;
  crashed: number;
  errored: number;
}

export interface ReleaseHealth {
  totals: SessionStats;
  releases: ReleaseRow[];
  series: SessionPoint[];
  environments: string[];
}

export interface TransactionRow {
  name: string;
  op: string;
  count: number;
  p50: number;
  p75: number;
  p95: number;
  failed: number;
  last_seen: string;
}

export interface TransactionList {
  transactions: TransactionRow[];
  ops: string[];
  environments: string[];
}

export interface TimingPoint {
  time: string;
  count: number;
  p50: number | null;
  p95: number | null;
}

export interface TransactionSample {
  event_id: string;
  started_at: string;
  duration_ms: number;
  status: string;
  release: string;
  environment: string;
  platform: string;
  spans: number;
}

export interface TransactionDetail {
  summary: TransactionRow;
  series: TimingPoint[];
  releases: { release: string; count: number; p50: number; p95: number }[];
  span_ops: { op: string; count: number; p50: number; p95: number; per_transaction: number }[];
  samples: TransactionSample[];
}

export interface SpanPayload {
  span_id: string;
  op: string;
  description?: string;
  status?: string;
  start_ms: number;
  duration_ms: number;
  data?: Record<string, unknown>;
}

export interface TransactionPayload {
  event_id: string;
  timestamp: string;
  platform: string;
  release?: string;
  environment: string;
  context?: Record<string, unknown>;
  transaction: {
    name: string;
    op: string;
    status: string;
    start_timestamp: string;
    duration_ms: number;
    measurements?: Record<string, number>;
    spans?: SpanPayload[];
  };
}

// The super admin view. Mirrors server/internal/store/admin.go.

export type AdminStatus = "active" | "quiet" | "no_events" | "near_quota" | "over_quota" | "no_projects";

export interface AdminProject {
  id: number;
  organization_id: number;
  organization: string;
  name: string;
  created_at: string;
  status: AdminStatus;
  unresolved: number;
  events_this_month: number;
  events_last_month: number;
  monthly_quota: number | null;
  rate_limit: number | null;
  errors_24h: number;
  sessions_24h: number;
  crashed_24h: number;
  last_event_at: string | null;
}

export interface AdminOrg {
  id: number;
  name: string;
  created_at: string;
  owner_email: string;
  members: number;
  projects: number;
  status: AdminStatus;
  unresolved: number;
  events_this_month: number;
  errors_24h: number;
  last_event_at: string | null;
}

export interface AdminOverview {
  generated_at: string;
  totals: {
    organizations: number;
    new_organizations_7d: number;
    projects: number;
    new_projects_7d: number;
    users: number;
    new_users_7d: number;
    events_this_month: number;
    events_last_month: number;
    errors_24h: number;
    unresolved: number;
    by_status: Partial<Record<AdminStatus, number>>;
  };
  organizations: AdminOrg[];
  projects: AdminProject[];
}

export interface AdminOrgDetail {
  organization: AdminOrg;
  projects: AdminProject[];
  members: Member[];
}
