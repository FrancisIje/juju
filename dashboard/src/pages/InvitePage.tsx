import { useEffect, useState, type FormEvent } from "react";
import { Pixels } from "../components/Pixels";
import { Logo } from "../components/Shell";
import { api, ApiError, type InviteInfo, type Me } from "../lib/api";
import { Link } from "../lib/router";

// What an invite link opens: who invited you to what, then one step to
// join, whether or not you already have an account.
export function InvitePage({ token, onJoined }: { token: string; onJoined: (orgId: number) => void }) {
  const [info, setInfo] = useState<InviteInfo | null>(null);
  const [gone, setGone] = useState("");
  const [me, setMe] = useState<Me | null | undefined>(undefined); // undefined: still checking
  const [form, setForm] = useState({ name: "", email: "", password: "" });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api<InviteInfo>(`/api/ui/invites/${encodeURIComponent(token)}`)
      .then(setInfo)
      .catch((err) => setGone(err instanceof ApiError ? err.message : "Couldn't load this invite."));
    api<Me>("/api/ui/me")
      .then(setMe)
      .catch(() => setMe(null));
  }, [token]);

  async function join() {
    setBusy(true);
    setError("");
    try {
      const r = await api<{ organization_id: number }>(`/api/ui/invites/${encodeURIComponent(token)}/accept`, { method: "POST" });
      onJoined(r.organization_id);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Couldn't join. Try again.");
      setBusy(false);
    }
  }

  async function signup(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      await api("/api/ui/auth/signup", { method: "POST", body: { ...form, invite: token } });
      onJoined(info!.organization_id);
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
      setBusy(false);
    }
  }

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });
  const next = `/login?next=${encodeURIComponent(`/invite/${token}`)}`;
  const capital = (s: string) => s.charAt(0).toUpperCase() + s.slice(1);

  let body;
  if (gone) {
    body = (
      <>
        <h1>This invite doesn't work any more</h1>
        <p className="muted">{capital(gone)}. Invite links work once and expire after 7 days.</p>
        <Link className="link" to="/login">
          Go to log in
        </Link>
      </>
    );
  } else if (!info || me === undefined) {
    return null;
  } else if (me) {
    const member = me.organizations.some((o) => o.id === info.organization_id);
    body = (
      <>
        <div style={{ display: "grid", gap: 6 }}>
          <h1>Join {info.organization}</h1>
          <p className="muted">
            {info.invited_by ? `${info.invited_by} invited you` : "You were invited"} to work on {info.organization}'s projects. You're logged in as {me.user.email}.
          </p>
        </div>
        {error && <p className="error-text" role="alert">{error}</p>}
        {member ? (
          <button className="btn primary" onClick={() => onJoined(info.organization_id)} style={{ height: 38, justifyContent: "center" }}>
            You're already in {info.organization}. Open it
          </button>
        ) : (
          <button className="btn primary" onClick={join} disabled={busy} style={{ height: 38, justifyContent: "center" }}>
            {busy ? "Joining…" : `Join ${info.organization}`}
          </button>
        )}
      </>
    );
  } else {
    body = (
      <>
        <div style={{ display: "grid", gap: 6 }}>
          <h1>Join {info.organization}</h1>
          <p className="muted">
            {info.invited_by ? `${info.invited_by} invited you` : "You were invited"} to work on {info.organization}'s projects. Create your account to join.
          </p>
        </div>
        <form onSubmit={signup} style={{ display: "grid", gap: "var(--space-4)" }} noValidate>
          <div className="field">
            <label htmlFor="name">Your name</label>
            <input id="name" className="input" autoComplete="name" value={form.name} onChange={set("name")} />
          </div>
          <div className="field">
            <label htmlFor="email">Email</label>
            <input id="email" className="input" type="email" autoComplete="email" required autoFocus value={form.email} onChange={set("email")} />
          </div>
          <div className="field">
            <label htmlFor="password">Password</label>
            <input id="password" className="input" type="password" autoComplete="new-password" required value={form.password} onChange={set("password")} />
            <span className="hint">At least 10 characters.</span>
          </div>
          {error && <p className="error-text" role="alert">{error}</p>}
          <button className="btn primary" type="submit" disabled={busy} style={{ height: 38, justifyContent: "center" }}>
            {busy ? "One moment…" : `Create account and join ${info.organization}`}
          </button>
        </form>
        <p className="muted auth-switch">
          Already have an account?{" "}
          <Link className="link" to={next}>
            Log in to join
          </Link>
        </p>
      </>
    );
  }

  return (
    <div className="auth">
      <div className="auth-form">
        <Logo />
        {body}
      </div>
      <div className="auth-art">
        <Pixels seed={23} />
        <p className="tagline">Every crash, grouped, with the line that caused it.</p>
      </div>
    </div>
  );
}
