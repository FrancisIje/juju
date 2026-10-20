import { api, ApiError, CSRF_HEADER, setUnauthorizedHandler } from "../src/lib/api";
import { fakeServer } from "./helpers";

test("every request carries the CSRF header and a JSON body", async () => {
  const { calls } = fakeServer({ "POST /api/ui/x": { ok: true } });
  await api("/api/ui/x", { method: "POST", body: { a: 1 } });
  expect(calls[0]!.headers[CSRF_HEADER]).toBe("1");
  expect(calls[0]!.body).toEqual({ a: 1 });
});

test("a 401 calls the unauthorized handler, except on auth endpoints", async () => {
  const onAuth = vi.fn();
  setUnauthorizedHandler(onAuth);
  vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "log in first" }), { status: 401 })));

  await expect(api("/api/ui/me")).rejects.toMatchObject({ status: 401, message: "log in first" });
  expect(onAuth).toHaveBeenCalledTimes(1);

  // A wrong password is also a 401, but must show the error, not redirect.
  await expect(api("/api/ui/auth/login", { method: "POST" })).rejects.toBeInstanceOf(ApiError);
  expect(onAuth).toHaveBeenCalledTimes(1);
});

test("network failure becomes a readable error", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => Promise.reject(new TypeError("Failed to fetch"))));
  await expect(api("/api/ui/me")).rejects.toMatchObject({ status: 0, message: expect.stringContaining("Can't reach") });
});

test("a server error without a JSON body still has a message", async () => {
  vi.stubGlobal("fetch", vi.fn(async () => new Response("oops", { status: 502 })));
  await expect(api("/api/ui/me")).rejects.toMatchObject({ status: 502, message: "The server answered 502." });
});
