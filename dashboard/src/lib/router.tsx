// A tiny router: the dashboard has a handful of URL patterns, which doesn't
// justify a routing library. Pages are real URLs (/p/3/issues/42), so they
// can be bookmarked, shared and opened in new tabs.
import { useSyncExternalStore, type AnchorHTMLAttributes, type MouseEvent } from "react";

const EVENT = "juju:navigate";

function subscribe(cb: () => void) {
  window.addEventListener("popstate", cb);
  window.addEventListener(EVENT, cb);
  return () => {
    window.removeEventListener("popstate", cb);
    window.removeEventListener(EVENT, cb);
  };
}

export function navigate(to: string, opts: { replace?: boolean } = {}) {
  if (to === location.pathname + location.search) return;
  history[opts.replace ? "replaceState" : "pushState"](null, "", to);
  window.dispatchEvent(new Event(EVENT));
}

/** The current path and query string; components re-render when it changes. */
export function useLocation(): { path: string; query: URLSearchParams } {
  const href = useSyncExternalStore(subscribe, () => location.pathname + location.search);
  const [path, search = ""] = href.split("?");
  return { path: path ?? "/", query: new URLSearchParams(search) };
}

/**
 * Where to go after logging in, from ?next=. Only a path on this site:
 * checking the text ("starts with /") isn't enough, because browsers read
 * "/\\evil.com" as "//evil.com", another site. So resolve it the way the
 * browser will and compare origins.
 */
export function safeNext(next: string | null, origin = location.origin): string {
  if (!next) return "/";
  try {
    const u = new URL(next, origin);
    return u.origin === origin ? u.pathname + u.search + u.hash : "/";
  } catch {
    return "/";
  }
}

/** match("/p/:project/issues/:issue", "/p/3/issues/42") → { project: "3", issue: "42" } */
export function match(pattern: string, path: string): Record<string, string> | null {
  const p = pattern.split("/").filter(Boolean);
  const s = path.split("/").filter(Boolean);
  if (p.length !== s.length) return null;
  const params: Record<string, string> = {};
  for (let i = 0; i < p.length; i++) {
    const part = p[i]!;
    if (part.startsWith(":")) params[part.slice(1)] = decodeURIComponent(s[i]!);
    else if (part !== s[i]) return null;
  }
  return params;
}

/** A link that navigates without reloading, but still opens in a new tab with ⌘/Ctrl-click. */
export function Link({ to, onClick, ...rest }: { to: string } & AnchorHTMLAttributes<HTMLAnchorElement>) {
  function handle(e: MouseEvent<HTMLAnchorElement>) {
    onClick?.(e);
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    e.preventDefault();
    navigate(to);
  }
  return <a href={to} onClick={handle} {...rest} />;
}
