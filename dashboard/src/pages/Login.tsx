import { useEffect, useState, type FormEvent } from "react";
import { Pixels } from "../components/Pixels";
import { Logo } from "../components/Shell";
import { api, ApiError } from "../lib/api";
import { Link } from "../lib/router";

type Setup = { needs_setup: boolean; signup_open: boolean };

// Log in at /login, create an account at /signup. On a server with no
// accounts yet, both show first-run setup: the very first visit should go
// straight to "create your account", wherever the link pointed.
export function Login({ page, onDone }: { page: "login" | "signup"; onDone: () => void }) {
  const [setup, setSetup] = useState<Setup | null>(null);
  const [form, setForm] = useState({ email: "", password: "", name: "", organization: "" });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api<Setup>("/api/ui/auth/setup")
      .then(setSetup)
      .catch(() => setSetup({ needs_setup: false, signup_open: false }));
  }, []);
  useEffect(() => setError(""), [page]);

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });

  if (!setup) return null;
  const first = setup.needs_setup;
  const signingUp = first || page === "signup";

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (signingUp) await api("/api/ui/auth/signup", { method: "POST", body: form });
      else await api("/api/ui/auth/login", { method: "POST", body: { email: form.email, password: form.password } });
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
    } finally {
      setBusy(false);
    }
  }

  return (
    <div className="auth">
      <div className="auth-form">
        <Logo />
        {signingUp && !setup.signup_open ? (
          <SignupClosed />
        ) : (
          <>
            <div style={{ display: "grid", gap: 6 }}>
              <h1>{first ? "Create your account" : signingUp ? "Create an account" : "Log in"}</h1>
              <p className="muted">
                {first
                  ? "You're the first person on this server, so you'll own it. You can add teammates later."
                  : signingUp
                    ? "You'll get your own organization, where you can create projects."
                    : "Errors from your apps, grouped, with the line that caused them."}
              </p>
            </div>
            <form onSubmit={submit} style={{ display: "grid", gap: "var(--space-4)" }} noValidate>
              {signingUp && (
                <div className="field">
                  <label htmlFor="name">Your name</label>
                  <input id="name" className="input" autoComplete="name" value={form.name} onChange={set("name")} />
                </div>
              )}
              <div className="field">
                <label htmlFor="email">Email</label>
                <input id="email" className="input" type="email" autoComplete="email" required autoFocus value={form.email} onChange={set("email")} />
              </div>
              <div className="field">
                <label htmlFor="password">Password</label>
                <input id="password" className="input" type="password" autoComplete={signingUp ? "new-password" : "current-password"} required value={form.password} onChange={set("password")} />
                {signingUp && <span className="hint">At least 10 characters.</span>}
              </div>
              {signingUp && (
                <div className="field">
                  <label htmlFor="organization">Organization</label>
                  <input id="organization" className="input" placeholder="Your company or team" required value={form.organization} onChange={set("organization")} />
                </div>
              )}
              {error && (
                <p className="error-text" role="alert">
                  {error}
                </p>
              )}
              <button className="btn primary" type="submit" disabled={busy} style={{ height: 38, justifyContent: "center" }}>
                {busy ? "One moment…" : signingUp ? "Create account" : "Log in"}
              </button>
            </form>
          </>
        )}
        {/* Always say how to get to the other page, so nobody is stuck. */}
        {!first && (
          <p className="muted auth-switch">
            {signingUp ? (
              <>
                Already have an account?{" "}
                <Link className="link" to="/login">
                  Log in
                </Link>
              </>
            ) : setup.signup_open ? (
              <>
                New here?{" "}
                <Link className="link" to="/signup">
                  Create an account
                </Link>
              </>
            ) : (
              <>
                No account?{" "}
                <Link className="link" to="/signup">
                  How to get one
                </Link>
              </>
            )}
          </p>
        )}
      </div>
      <div className="auth-art">
        <Pixels seed={11} />
        <p className="tagline">Every crash, grouped, with the line that caused it.</p>
      </div>
    </div>
  );
}

// Sign-up is off (the default after the first account), so say who can add
// people and how, instead of showing a form that can only fail.
function SignupClosed() {
  return (
    <div style={{ display: "grid", gap: "var(--space-3)" }}>
      <h1>Ask for an account</h1>
      <p className="muted">This juju server doesn't allow sign-up. Ask the person who runs it to add you. They can do it from the server's shell:</p>
      <pre className="code" style={{ whiteSpace: "pre-wrap", overflowWrap: "anywhere" }}>
        juju-admin user create -email you@example.com -org Acme
      </pre>
      <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
        Or they can open sign-up for everyone by setting JUJU_ALLOW_SIGNUP=true.
      </p>
    </div>
  );
}
