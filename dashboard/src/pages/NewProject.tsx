import { useState, type FormEvent } from "react";
import { api, ApiError, type ProjectInfo } from "../lib/api";
import { useApp } from "../lib/app";
import { navigate } from "../lib/router";
import { rememberSecret } from "../lib/secrets";

export function NewProject({ orgId }: { orgId: number }) {
  const { me, reloadMe } = useApp();
  const org = me.organizations.find((o) => o.id === orgId) ?? me.organizations[0];
  const [name, setName] = useState("");
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  async function submit(e: FormEvent) {
    e.preventDefault();
    if (!org) return;
    setBusy(true);
    setError("");
    try {
      const p = await api<ProjectInfo>(`/api/ui/orgs/${org.id}/projects`, { method: "POST", body: { name } });
      // The secret key is shown once, on the next page, then never again.
      if (p.secret_key) rememberSecret(p.id, p.secret_key);
      await reloadMe();
      navigate(`/p/${p.id}/settings/setup`);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Couldn't create the project.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="page" style={{ maxWidth: 560 }}>
      <h1>New project</h1>
      <p className="muted">A project is one app: your website, your iOS app, your Android app. Each gets its own DSN and its own issues.</p>
      <form className="panel" onSubmit={submit} style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-4)" }}>
        <div className="field">
          <label htmlFor="project-name">Project name</label>
          <input id="project-name" className="input" autoFocus placeholder="shop-web" value={name} onChange={(e) => setName(e.target.value)} />
          <span className="hint">In {org?.name}. Short and recognizable, like shop-web or shop-ios.</span>
        </div>
        {error && (
          <p className="error-text" role="alert">
            {error}
          </p>
        )}
        <div className="toolbar">
          <button className="btn primary" disabled={busy || !name.trim()}>
            Create project
          </button>
          <button type="button" className="btn ghost" onClick={() => history.back()}>
            Cancel
          </button>
        </div>
      </form>
    </div>
  );
}
