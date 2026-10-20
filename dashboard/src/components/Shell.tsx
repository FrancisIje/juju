import { useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { api } from "../lib/api";
import { useApp } from "../lib/app";
import { isTyping, useShortcuts } from "../lib/keys";
import { Link, navigate, useLocation } from "../lib/router";
import { applyTheme, loadTheme, nextTheme, type Theme } from "../lib/theme";
import { CommandPalette, type Command } from "./CommandPalette";
import { ShortcutsHelp } from "./ShortcutsHelp";

export function Logo() {
  // The mark is a small pixel cluster in the accent color.
  return (
    <span className="brand-row">
      <svg width="18" height="18" viewBox="0 0 6 6" aria-hidden="true">
        <rect x="0" y="0" width="2" height="2" fill="var(--pixel-1)" />
        <rect x="2" y="2" width="2" height="2" fill="var(--pixel-2)" />
        <rect x="4" y="0" width="2" height="2" fill="var(--pixel-3)" />
        <rect x="0" y="4" width="2" height="2" fill="var(--pixel-3)" />
        <rect x="4" y="4" width="2" height="2" fill="var(--pixel-1)" />
      </svg>
      juju
    </span>
  );
}

export function Shell({ projectId, children }: { projectId: number | null; children: ReactNode }) {
  const { me, toast } = useApp();
  const { path } = useLocation();
  const [palette, setPalette] = useState(false);
  const [help, setHelp] = useState(false);
  const [menu, setMenu] = useState(false);
  const [theme, setTheme] = useState<Theme>(loadTheme);
  const pendingG = useRef(0);

  const projects = me.organizations.flatMap((o) => o.projects.map((p) => ({ ...p, org: o.name, orgId: o.id })));
  const current = projects.find((p) => p.id === projectId);
  const base = projectId ? `/p/${projectId}` : "";

  function cycleTheme() {
    const t = nextTheme(theme);
    setTheme(t);
    applyTheme(t);
    toast(t === "system" ? "Theme follows your system" : `${t === "dark" ? "Dark" : "Light"} theme`);
  }

  async function logout() {
    await api("/api/ui/auth/logout", { method: "POST" }).catch(() => {});
    location.assign("/login");
  }

  const commands = useMemo<Command[]>(() => {
    const list: Command[] = [];
    if (projectId) {
      list.push(
        { id: "go-issues", group: "Go to", label: "Issues", hint: "g i", run: () => navigate(`${base}/issues`) },
        { id: "go-setup", group: "Go to", label: "Settings: Setup and DSN", hint: "g s", run: () => navigate(`${base}/settings/setup`) },
        { id: "go-alerts", group: "Go to", label: "Settings: Alerts", run: () => navigate(`${base}/settings/alerts`) },
        { id: "go-keys", group: "Go to", label: "Settings: Keys", run: () => navigate(`${base}/settings/keys`) },
        { id: "go-files", group: "Go to", label: "Settings: Debug files", run: () => navigate(`${base}/settings/files`) },
      );
    }
    for (const p of projects) {
      if (p.id !== projectId) list.push({ id: `proj-${p.id}`, group: "Switch project", label: `${p.org} / ${p.name}`, run: () => navigate(`/p/${p.id}/issues`) });
    }
    for (const o of me.organizations) list.push({ id: `new-${o.id}`, group: "Create", label: `New project in ${o.name}`, run: () => navigate(`/new?org=${o.id}`) });
    list.push(
      { id: "theme", group: "Preferences", label: "Change theme (system, dark, light)", run: cycleTheme },
      { id: "help", group: "Help", label: "Keyboard shortcuts", hint: "?", run: () => setHelp(true) },
      { id: "logout", group: "Account", label: "Log out", run: logout },
    );
    return list;
    // eslint-disable-next-line react-hooks/exhaustive-deps
  }, [projectId, me, theme]);

  useShortcuts({ "mod+k": () => setPalette(true), "?": () => setHelp(true) }, !palette && !help);

  // "g i" and "g s": two-key sequences. This listener uses the capture phase,
  // so it runs before the pages' own shortcuts and can keep the issue list
  // from also treating that "i" as "ignore".
  useEffect(() => {
    if (palette || help) return;
    function onKey(e: KeyboardEvent) {
      if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target)) return;
      if (e.key === "g") {
        pendingG.current = Date.now();
        return;
      }
      const go = Date.now() - pendingG.current < 800 && base ? { i: `${base}/issues`, s: `${base}/settings/setup` }[e.key] : undefined;
      pendingG.current = 0;
      if (go) {
        e.preventDefault();
        e.stopImmediatePropagation();
        navigate(go);
      }
    }
    window.addEventListener("keydown", onKey, true);
    return () => window.removeEventListener("keydown", onKey, true);
  }, [palette, help, base]);

  const nav = (to: string, label: string, count?: number) => (
    <Link to={to} aria-current={path.startsWith(to) ? "page" : undefined} onClick={() => setMenu(false)}>
      <span>{label}</span>
      {count !== undefined && <span className="count">{count}</span>}
    </Link>
  );

  return (
    <div className="shell">
      <div className="mobile-bar">
        <Logo />
        <button className="btn small" onClick={() => setMenu(true)} aria-expanded={menu}>
          Menu
        </button>
      </div>
      <aside className={`sidebar${menu ? " open" : ""}`} aria-label="Navigation">
        <div className="page-head">
          <Logo />
          {menu && (
            <button className="btn ghost small" onClick={() => setMenu(false)}>
              Close
            </button>
          )}
        </div>
        <button className="btn" style={{ justifyContent: "space-between" }} onClick={() => setPalette(true)}>
          <span className="muted">Search or jump…</span>
          <kbd>⌘K</kbd>
        </button>
        {current && (
          <div className="nav">
            <span className="nav-label">{current.org}</span>
            {nav(`${base}/issues`, current.name, current.unresolved)}
            {nav(`${base}/settings`, "Settings")}
          </div>
        )}
        {projects.length > 1 && (
          <div className="nav">
            <span className="nav-label">Other projects</span>
            {projects
              .filter((p) => p.id !== projectId)
              .map((p) => (
                <Link key={p.id} to={`/p/${p.id}/issues`} onClick={() => setMenu(false)}>
                  <span>{p.name}</span>
                  <span className="count">{p.unresolved}</span>
                </Link>
              ))}
          </div>
        )}
        <div className="nav">
          {me.organizations[0] && (
            <Link to={`/new?org=${me.organizations[0].id}`} onClick={() => setMenu(false)}>
              <span>+ New project</span>
            </Link>
          )}
        </div>
        <div className="sidebar-foot">
          <div className="divider" />
          <span className="faint" style={{ fontSize: "var(--text-xs)", padding: "0 var(--space-2)" }}>
            {me.user.email}
          </span>
          <div className="toolbar">
            <button className="btn ghost small" onClick={cycleTheme} title="Theme: system, dark or light">
              Theme: {theme}
            </button>
            <button className="btn ghost small" onClick={() => setHelp(true)}>
              Shortcuts <kbd>?</kbd>
            </button>
            <button className="btn ghost small" onClick={logout}>
              Log out
            </button>
          </div>
        </div>
      </aside>
      <main className="main" id="main">
        {children}
      </main>
      {palette && <CommandPalette commands={commands} projectId={projectId} onClose={() => setPalette(false)} />}
      {help && <ShortcutsHelp onClose={() => setHelp(false)} />}
    </div>
  );
}
