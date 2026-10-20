import { useCallback, useEffect, useMemo, useRef, useState } from "react";
import { Shell } from "./components/Shell";
import { api, ApiError, setUnauthorizedHandler, type Me } from "./lib/api";
import { AppContext, type AppState } from "./lib/app";
import { match, navigate, useLocation } from "./lib/router";
import { Issue } from "./pages/Issue";
import { Issues } from "./pages/Issues";
import { Login } from "./pages/Login";
import { NewProject } from "./pages/NewProject";
import { Settings, type SettingsTab } from "./pages/Settings";

const settingsTabs: SettingsTab[] = ["setup", "keys", "alerts", "files"];

export function App() {
  const { path, query } = useLocation();
  const [me, setMe] = useState<Me | null>(null);
  const [failed, setFailed] = useState<ApiError | null>(null);
  const [message, setMessage] = useState("");
  const timer = useRef<ReturnType<typeof setTimeout>>(undefined);

  const reloadMe = useCallback(async () => {
    try {
      setMe(await api<Me>("/api/ui/me"));
      setFailed(null);
    } catch (err) {
      // A 401 is handled by the unauthorized handler (it goes to /login).
      if (err instanceof ApiError && err.status !== 401) setFailed(err);
    }
  }, []);

  useEffect(() => {
    // Any request that finds the session gone sends people to log in, and
    // brings them back to the page they were on afterwards.
    setUnauthorizedHandler(() => {
      setMe(null);
      const back = location.pathname + location.search;
      if (location.pathname !== "/login") navigate(`/login${back !== "/" ? `?next=${encodeURIComponent(back)}` : ""}`, { replace: true });
    });
    if (location.pathname !== "/login") void reloadMe();
  }, [reloadMe]);

  const toast = useCallback((m: string) => {
    setMessage(m);
    clearTimeout(timer.current);
    timer.current = setTimeout(() => setMessage(""), 3500);
  }, []);

  const state = useMemo<AppState | null>(() => (me ? { me, reloadMe, toast } : null), [me, reloadMe, toast]);

  // "/" and old links go to the first project, or to creating one.
  const first = me?.organizations.flatMap((o) => o.projects)[0];
  useEffect(() => {
    if (!me || path !== "/") return;
    if (first) navigate(`/p/${first.id}/issues`, { replace: true });
    else if (me.organizations[0]) navigate(`/new?org=${me.organizations[0].id}`, { replace: true });
  }, [me, path, first]);

  if (path === "/login") {
    return (
      <Login
        onDone={async () => {
          await reloadMe();
          const next = query.get("next");
          // Only same-site paths, so a crafted link can't bounce people elsewhere.
          navigate(next && next.startsWith("/") && !next.startsWith("//") ? next : "/", { replace: true });
        }}
      />
    );
  }
  if (failed) {
    return (
      <div className="empty" style={{ minHeight: "100%", alignContent: "center" }}>
        <h1>juju can't load</h1>
        <p className="muted">{failed.message}</p>
        <button className="btn" onClick={() => void reloadMe()}>
          Try again
        </button>
      </div>
    );
  }
  if (!state) return null;

  return (
    <AppContext.Provider value={state}>
      <Routed path={path} query={query} />
      {message && (
        <div className="toast" role="status">
          {message}
        </div>
      )}
    </AppContext.Provider>
  );
}

function Routed({ path, query }: { path: string; query: URLSearchParams }) {
  let m: Record<string, string> | null;
  if ((m = match("/p/:project/issues/:issue", path))) {
    return (
      <Shell projectId={Number(m.project)}>
        <Issue key={m.issue} projectId={Number(m.project)} issueId={Number(m.issue)} />
      </Shell>
    );
  }
  if ((m = match("/p/:project/issues", path))) {
    return (
      <Shell projectId={Number(m.project)}>
        <Issues key={m.project} projectId={Number(m.project)} />
      </Shell>
    );
  }
  if ((m = match("/p/:project/settings/:tab", path)) || (m = match("/p/:project/settings", path))) {
    const tab = settingsTabs.includes(m.tab as SettingsTab) ? (m.tab as SettingsTab) : "setup";
    return (
      <Shell projectId={Number(m.project)}>
        <Settings key={m.project} projectId={Number(m.project)} tab={tab} />
      </Shell>
    );
  }
  if (path === "/new") {
    return (
      <Shell projectId={null}>
        <NewProject orgId={Number(query.get("org"))} />
      </Shell>
    );
  }
  if (path === "/") return null;
  return (
    <Shell projectId={null}>
      <div className="empty">
        <h1>Page not found</h1>
        <p className="muted">There's nothing at {path}.</p>
        <button className="btn" onClick={() => navigate("/")}>
          Go to your issues
        </button>
      </div>
    </Shell>
  );
}
