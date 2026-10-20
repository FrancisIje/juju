import { render } from "@testing-library/react";
import type { ReactNode } from "react";
import type { Me } from "../src/lib/api";
import { AppContext } from "../src/lib/app";

export const me: Me = {
  user: { id: 1, email: "ada@example.com", name: "Ada" },
  organizations: [{ id: 1, name: "Acme", role: "owner", projects: [{ id: 3, name: "shop-web", unresolved: 2 }] }],
};

/** A fake server: each route is "METHOD /path" (query string ignored) → JSON body. Records every call. */
export function fakeServer(routes: Record<string, unknown | ((body: unknown) => unknown)>) {
  const calls: { method: string; path: string; headers: Record<string, string>; body: unknown }[] = [];
  const fetchMock = vi.fn(async (input: RequestInfo | URL, init: RequestInit = {}) => {
    const url = new URL(String(input), "http://localhost");
    const method = init.method ?? "GET";
    const body = init.body ? JSON.parse(String(init.body)) : undefined;
    calls.push({ method, path: url.pathname + url.search, headers: init.headers as Record<string, string>, body });
    const route = routes[`${method} ${url.pathname}`];
    if (route === undefined) return new Response(JSON.stringify({ error: "not found" }), { status: 404 });
    const value = typeof route === "function" ? (route as (b: unknown) => unknown)(body) : route;
    return new Response(JSON.stringify(value), { status: 200, headers: { "Content-Type": "application/json" } });
  });
  vi.stubGlobal("fetch", fetchMock);
  return { calls, fetchMock };
}

export function renderWithApp(ui: ReactNode, toast = vi.fn()) {
  return { toast, ...render(<AppContext.Provider value={{ me, reloadMe: async () => {}, toast }}>{ui}</AppContext.Provider>) };
}
