import { useEffect, useState, type FormEvent } from "react";
import { Code, CopyButton } from "../components/CopyButton";
import { api, ApiError, type AlertsView, type DebugFile, type ProjectInfo } from "../lib/api";
import { useApp } from "../lib/app";
import { Link } from "../lib/router";
import { takeSecret } from "../lib/secrets";
import { platforms, snippet, type Platform } from "../lib/snippets";
import { bytes, exact, relative } from "../lib/time";
import { useApi } from "../lib/useApi";
import { Team } from "./Team";

export type SettingsTab = "setup" | "keys" | "alerts" | "files" | "team";
const tabs: { id: SettingsTab; label: string }[] = [
  { id: "setup", label: "Setup" },
  { id: "keys", label: "Keys" },
  { id: "alerts", label: "Alerts" },
  { id: "files", label: "Debug files" },
  { id: "team", label: "Team" },
];

export function Settings({ projectId, tab }: { projectId: number; tab: SettingsTab }) {
  const project = useApi<{ project: ProjectInfo }>(`/api/ui/projects/${projectId}`);
  // The one-time secret is taken in an effect, not in useState's initializer:
  // React may call initializers twice in development, and the second call
  // would find it already taken. App keys this page by project, so the
  // secret never carries over to another project.
  const [secret, setSecret] = useState<string>();
  useEffect(() => {
    const s = takeSecret(projectId);
    if (s) setSecret(s);
  }, [projectId]);

  const p = project.data?.project;
  const { me } = useApp();
  // The team is the organization that owns this project.
  const org = me.organizations.find((o) => o.projects.some((pr) => pr.id === projectId));
  return (
    <div className="page" style={{ maxWidth: 920 }}>
      <div className="page-head">
        <h1>Settings{p && <span className="muted" style={{ fontWeight: 400 }}> · {p.name}</span>}</h1>
      </div>
      <div className="tabs" role="tablist" aria-label="Settings">
        {tabs.map((t) => (
          <Link key={t.id} to={`/p/${projectId}/settings/${t.id}`} role="tab" aria-selected={tab === t.id} className="tab-link">
            {t.label}
          </Link>
        ))}
      </div>
      {project.error && (
        <div className="banner warn" role="alert">
          {project.error.message}
        </div>
      )}
      {p && tab === "setup" && <Setup project={p} secret={secret} />}
      {p && tab === "keys" && <Keys project={p} secret={secret} onSecret={setSecret} />}
      {tab === "alerts" && <Alerts projectId={projectId} />}
      {p && tab === "files" && <Files project={p} />}
      {tab === "team" && org && <Team org={org} />}
    </div>
  );
}

function SecretOnce({ secret }: { secret: string }) {
  return (
    <div className="banner ok" role="status" style={{ display: "grid", gap: "var(--space-2)" }}>
      <strong>Your secret key. Copy it now: it's shown only this once.</strong>
      <span>Use it for the read API and for uploading debug files from CI. Unlike the DSN, keep it out of your apps.</span>
      <Code>{secret}</Code>
    </div>
  );
}

function Setup({ project, secret }: { project: ProjectInfo; secret?: string }) {
  const [platform, setPlatform] = useState<Platform>("web");
  const s = snippet(platform, project.dsn);
  return (
    <div style={{ display: "grid", gap: "var(--space-5)" }}>
      {secret && <SecretOnce secret={secret} />}
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <h2>DSN</h2>
        <p className="muted">The address your app sends errors to. It's safe to ship inside apps: it can send errors, but not read them.</p>
        <Code>{project.dsn}</Code>
      </section>
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <h2>Connect an app</h2>
        <div className="tabs" role="tablist" aria-label="Platform">
          {platforms.map((pl) => (
            <button key={pl.id} role="tab" aria-selected={platform === pl.id} onClick={() => setPlatform(pl.id)}>
              {pl.label}
            </button>
          ))}
        </div>
        {s.install && <Code>{s.install}</Code>}
        <Code>{s.code}</Code>
      </section>
    </div>
  );
}

function Keys({ project, secret, onSecret }: { project: ProjectInfo; secret?: string; onSecret: (s: string) => void }) {
  const { toast } = useApp();
  const [confirming, setConfirming] = useState(false);
  const [busy, setBusy] = useState(false);

  async function rotate() {
    setBusy(true);
    try {
      const r = await api<{ secret_key: string }>(`/api/ui/projects/${project.id}/rotate-secret`, { method: "POST" });
      onSecret(r.secret_key);
      setConfirming(false);
      toast("New secret key created. The old one no longer works.");
    } catch (err) {
      toast(err instanceof ApiError ? err.message : "Couldn't rotate the key.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div style={{ display: "grid", gap: "var(--space-5)" }}>
      {secret && <SecretOnce secret={secret} />}
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <h2>Public key</h2>
        <p className="muted">Part of the DSN. Lets apps send errors to this project, nothing more.</p>
        <div className="code-wrap">
          <pre className="code">{project.public_key}</pre>
          <CopyButton text={project.public_key} />
        </div>
      </section>
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <h2>Secret key</h2>
        <p className="muted">
          Reads issues through the API and uploads debug files. The server stores only a hash of it, so it can't be shown again. If you lost it or it leaked, make a new one.
        </p>
        {!confirming ? (
          <button className="btn" style={{ justifySelf: "start" }} onClick={() => setConfirming(true)}>
            Rotate secret key…
          </button>
        ) : (
          <div className="banner warn" role="alertdialog" aria-label="Confirm rotation" style={{ display: "grid", gap: "var(--space-3)" }}>
            <span>The current secret key stops working immediately. Anything using it (CI uploads, scripts) needs the new one.</span>
            <div className="toolbar">
              <button className="btn danger" onClick={rotate} disabled={busy}>
                {busy ? "Rotating…" : "Yes, rotate it"}
              </button>
              <button className="btn ghost" onClick={() => setConfirming(false)}>
                Cancel
              </button>
            </div>
          </div>
        )}
      </section>
    </div>
  );
}

function Alerts({ projectId }: { projectId: number }) {
  const { toast } = useApp();
  const { data, error, setData, reload } = useApi<AlertsView>(`/api/ui/projects/${projectId}/alerts`);
  const [webhook, setWebhook] = useState("");
  const [emails, setEmails] = useState("");
  const [onNew, setOnNew] = useState(true);
  const [onRegression, setOnRegression] = useState(true);
  const [formError, setFormError] = useState("");
  const [busy, setBusy] = useState(false);
  const [test, setTest] = useState<Record<string, string> | null>(null);

  useEffect(() => {
    if (!data) return;
    setEmails(data.email_to.join(", "));
    setOnNew(data.on_new_issue);
    setOnRegression(data.on_regression);
  }, [data]);

  async function save(e: FormEvent, removeWebhook = false) {
    e.preventDefault();
    setBusy(true);
    setFormError("");
    try {
      const body: Record<string, unknown> = {
        email_to: emails.split(/[\s,;]+/).filter(Boolean),
        on_new_issue: onNew,
        on_regression: onRegression,
      };
      // Leaving the webhook field empty keeps the saved one; the server
      // never sends it back, so there's nothing to pre-fill.
      if (removeWebhook) body.slack_webhook_url = "";
      else if (webhook.trim()) body.slack_webhook_url = webhook.trim();
      const v = await api<AlertsView>(`/api/ui/projects/${projectId}/alerts`, { method: "PUT", body });
      setData(v);
      setWebhook("");
      toast("Alert settings saved");
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Couldn't save.");
    } finally {
      setBusy(false);
    }
  }

  async function sendTest() {
    setTest(null);
    setFormError("");
    try {
      const r = await api<{ results: Record<string, string> }>(`/api/ui/projects/${projectId}/alerts/test`, { method: "POST" });
      setTest(r.results);
      void reload(true);
    } catch (err) {
      setFormError(err instanceof ApiError ? err.message : "Couldn't send a test.");
    }
  }

  if (error) return <div className="banner warn" role="alert">{error.message}</div>;
  if (!data) return <p className="muted">Loading…</p>;
  return (
    <div style={{ display: "grid", gap: "var(--space-5)" }}>
      <form className="panel" onSubmit={save} style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-4)" }}>
        <div style={{ display: "grid", gap: 4 }}>
          <h2>When to alert</h2>
          <p className="muted">One message per issue, not per event, so a crash loop doesn't flood your channel.</p>
        </div>
        <label className="switch">
          <input type="checkbox" checked={onNew} onChange={(e) => setOnNew(e.target.checked)} />A new kind of error appears
        </label>
        <label className="switch">
          <input type="checkbox" checked={onRegression} onChange={(e) => setOnRegression(e.target.checked)} />A resolved issue happens again
        </label>
        <div className="divider" />
        <div className="field">
          <label htmlFor="webhook">Slack incoming webhook</label>
          <input
            id="webhook"
            className="input mono"
            type="url"
            autoComplete="off"
            placeholder={data.slack_webhook_set ? `Saved (${data.slack_webhook_hint}). Paste a new one to replace it.` : "https://hooks.slack.com/services/…"}
            value={webhook}
            onChange={(e) => setWebhook(e.target.value)}
          />
          <span className="hint">
            In Slack: Apps → Incoming Webhooks → Add to a channel. Treat the URL like a password.
            {data.slack_webhook_set && (
              <>
                {" "}
                <button type="button" className="btn ghost small" onClick={(e) => void save(e, true)}>
                  Remove saved webhook
                </button>
              </>
            )}
          </span>
        </div>
        <div className="field">
          <label htmlFor="emails">Email to</label>
          <input id="emails" className="input" placeholder="you@example.com, team@example.com" value={emails} onChange={(e) => setEmails(e.target.value)} disabled={!data.email_available && !emails} />
          <span className="hint">{data.email_available ? "Separate addresses with commas." : "Email needs SMTP settings on the server (JUJU_SMTP_ADDR). Slack works without it."}</span>
        </div>
        {formError && (
          <p className="error-text" role="alert">
            {formError}
          </p>
        )}
        {test && (
          <div className="banner" role="status" style={{ display: "grid", gap: 4 }}>
            {Object.entries(test).map(([channel, result]) => (
              <span key={channel}>
                <span className={`chip ${result === "sent" ? "resolved" : "unresolved"}`}>{result === "sent" ? "Sent" : "Failed"}</span> {channel}
                {result !== "sent" && <span className="muted">: {result}</span>}
              </span>
            ))}
          </div>
        )}
        <div className="toolbar">
          <button className="btn primary" disabled={busy}>
            Save
          </button>
          <button type="button" className="btn" onClick={sendTest} disabled={!data.slack_webhook_set && data.email_to.length === 0} title="Uses the saved settings">
            Send a test alert
          </button>
        </div>
      </form>

      <section className="panel" style={{ overflow: "hidden" }}>
        <h2 style={{ padding: "var(--space-4) var(--space-4) var(--space-2)" }}>Recent deliveries</h2>
        {data.deliveries.length === 0 ? (
          <p className="muted" style={{ padding: "0 var(--space-4) var(--space-4)" }}>
            Nothing sent yet. Each alert shows up here with whether it got through, which is the first place to look if one didn't arrive.
          </p>
        ) : (
          <table className="table">
            <thead>
              <tr>
                <th>Result</th>
                <th>Why</th>
                <th>Where</th>
                <th>When</th>
              </tr>
            </thead>
            <tbody>
              {data.deliveries.map((d) => (
                <tr key={d.id}>
                  <td>
                    <span className={`chip ${d.ok ? "resolved" : "unresolved"}`}>{d.ok ? "Sent" : "Failed"}</span>
                  </td>
                  <td>
                    {d.issue_id ? <Link className="link" to={`/p/${projectId}/issues/${d.issue_id}`}>{d.kind.replace("_", " ")}</Link> : d.kind.replace("_", " ")}
                    {d.error && <div className="faint mono" style={{ fontSize: "var(--text-xs)" }}>{d.error}</div>}
                  </td>
                  <td>{d.channel}</td>
                  <td title={exact(d.created_at)}>{relative(d.created_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        )}
      </section>
    </div>
  );
}

function Files({ project }: { project: ProjectInfo }) {
  const { data, error } = useApi<{ files: DebugFile[] }>(`/api/ui/projects/${project.id}/files`);
  const origin = window.location.origin;
  const upload = `export JUJU_URL=${origin} JUJU_PROJECT=${project.id} JUJU_SECRET_KEY=<your secret key>

# Web: give each bundle a debug id (before deploying), then upload its map
juju sourcemaps inject dist/
juju upload dist/

# React Native: with withJujuConfig in metro.config.js, the release build's map
juju upload android/app/build/generated/sourcemaps/react/release/index.android.bundle.map

# Flutter: built with --obfuscate --split-debug-info=build/symbols
juju upload -kind dart_symbols build/symbols/`;
  return (
    <div style={{ display: "grid", gap: "var(--space-5)" }}>
      <section className="panel" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-3)" }}>
        <h2>Readable stack traces for release builds</h2>
        <p className="muted">
          Release builds are minified or obfuscated, so their stack traces point at code nobody wrote. Upload the debug files from each build and juju maps frames back to your
          source. Run this in CI after the build. Each bundle's debug id names its own map, so the release can be called anything; files uploaded with{" "}
          <code>-release</code> and no debug id must match the release your SDK sends.
        </p>
        <Code>{upload}</Code>
      </section>
      <section className="panel" style={{ overflow: "hidden" }}>
        <h2 style={{ padding: "var(--space-4) var(--space-4) var(--space-2)" }}>Uploaded files</h2>
        {error && <p className="error-text" style={{ padding: "0 var(--space-4)" }}>{error.message}</p>}
        {data && data.files.length === 0 && (
          <p className="muted" style={{ padding: "0 var(--space-4) var(--space-4)" }}>
            None yet. Errors still arrive without them, just with minified frames.
          </p>
        )}
        {data && data.files.length > 0 && (
          // Names are debug ids and build ids: kept on one line so they stay
          // readable and copyable, with the table scrolling inside its box on
          // a phone instead of breaking an id every few characters.
          <div style={{ overflowX: "auto" }}>
            <table className="table">
              <thead>
                <tr>
                  <th>Name</th>
                  <th>Kind</th>
                  <th>Release</th>
                  <th className="num">Size</th>
                  <th>Uploaded</th>
                </tr>
              </thead>
              <tbody>
                {data.files.map((f) => (
                  <tr key={f.id}>
                    <td className="mono" style={{ whiteSpace: "nowrap" }}>{f.name}</td>
                    <td>{f.kind === "dart_symbols" ? "Dart symbols" : "Source map"}</td>
                    <td>{f.release ?? <span className="faint">any ({f.kind === "dart_symbols" ? "build id" : "debug id"})</span>}</td>
                    <td className="num">{bytes(f.size)}</td>
                    <td title={exact(f.created_at)}>{relative(f.created_at)}</td>
                  </tr>
                ))}
              </tbody>
            </table>
          </div>
        )}
      </section>
    </div>
  );
}
