import { useEffect, useState, type FormEvent } from "react";
import { Pixels } from "../components/Pixels";
import { Logo } from "../components/Shell";
import { api, ApiError } from "../lib/api";

// Login, and first-run setup when the server has no accounts yet. One page,
// because the very first visit should go straight to "create your account".
export function Login({ onDone }: { onDone: () => void }) {
  const [mode, setMode] = useState<"login" | "setup" | null>(null);
  const [signupOpen, setSignupOpen] = useState(false);
  const [form, setForm] = useState({ email: "", password: "", name: "", organization: "" });
  const [error, setError] = useState("");
  const [busy, setBusy] = useState(false);

  useEffect(() => {
    api<{ needs_setup: boolean; signup_open: boolean }>("/api/ui/auth/setup")
      .then((s) => {
        setMode(s.needs_setup ? "setup" : "login");
        setSignupOpen(s.signup_open);
      })
      .catch(() => setMode("login"));
  }, []);

  const set = (k: keyof typeof form) => (e: React.ChangeEvent<HTMLInputElement>) => setForm({ ...form, [k]: e.target.value });

  async function submit(e: FormEvent) {
    e.preventDefault();
    setBusy(true);
    setError("");
    try {
      if (mode === "setup") await api("/api/ui/auth/signup", { method: "POST", body: form });
      else await api("/api/ui/auth/login", { method: "POST", body: { email: form.email, password: form.password } });
      onDone();
    } catch (err) {
      setError(err instanceof ApiError ? err.message : "Something went wrong. Try again.");
    } finally {
      setBusy(false);
    }
  }

  if (!mode) return null;
  const setup = mode === "setup";
  return (
    <div className="auth">
      <div className="auth-form">
        <Logo />
        <div style={{ display: "grid", gap: 6 }}>
          <h1>{setup ? "Create your account" : "Log in"}</h1>
          <p className="muted">{setup ? "You're the first person on this server, so you'll own it. You can add teammates later." : "Errors from your apps, grouped, with the line that caused them."}</p>
        </div>
        <form onSubmit={submit} style={{ display: "grid", gap: "var(--space-4)" }} noValidate>
          {setup && (
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
            <input id="password" className="input" type="password" autoComplete={setup ? "new-password" : "current-password"} required value={form.password} onChange={set("password")} />
            {setup && <span className="hint">At least 10 characters.</span>}
          </div>
          {setup && (
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
            {busy ? "One moment…" : setup ? "Create account" : "Log in"}
          </button>
        </form>
        {!setup && signupOpen && (
          <button className="btn ghost" onClick={() => setMode("setup")}>
            No account? Create one
          </button>
        )}
      </div>
      <div className="auth-art">
        <Pixels seed={11} />
        <p className="tagline">Every crash, grouped, with the line that caused it.</p>
      </div>
    </div>
  );
}
