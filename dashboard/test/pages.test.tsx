import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import { CSRF_HEADER, type EventPayload, type IssueRow } from "../src/lib/api";
import { StackTrace } from "../src/components/issue/StackTrace";
import { Issue } from "../src/pages/Issue";
import { Issues } from "../src/pages/Issues";
import { Onboarding } from "../src/pages/Onboarding";
import { fakeServer, renderWithApp } from "./helpers";

const row = (id: number, title: string, extra: Partial<IssueRow> = {}): IssueRow => ({
  id,
  title,
  culprit: "render (app.js)",
  status: "unresolved",
  platform: "javascript",
  last_release: "web@1.0.0",
  event_count: 3,
  user_count: 1,
  first_seen: new Date().toISOString(),
  last_seen: new Date().toISOString(),
  status_changed_at: null,
  regressed: false,
  trend: new Array(24).fill(0),
  ...extra,
});

const project = { project: { id: 3, name: "shop-web", organization: "Acme", public_key: "pk_abc", dsn: "http://pk_abc@localhost:8080/3" }, counts: { unresolved: 2, resolved: 0, ignored: 0 }, environments: ["production"] };

beforeEach(() => history.replaceState(null, "", "/p/3/issues"));

describe("issue list", () => {
  // Remembers status changes, like the real server, so the reload after a
  // change returns what a person would actually see.
  function server() {
    let issues = [row(10, "TypeError: a is undefined"), row(11, "RangeError: too deep", { regressed: true })];
    return fakeServer({
      "GET /api/ui/projects/3": project,
      "GET /api/ui/projects/3/issues": () => {
        const open = issues.filter((i) => i.status === "unresolved");
        return { issues: open, total: open.length, counts: project.counts };
      },
      "POST /api/ui/projects/3/issues/status": (body: unknown) => {
        const { ids, status } = body as { ids: number[]; status: IssueRow["status"] };
        issues = issues.map((i) => (ids.includes(i.id) ? { ...i, status } : i));
        return { changed: ids.length };
      },
    });
  }

  test("j then r resolves the second issue, with the CSRF header", async () => {
    const { calls } = server();
    const { toast } = renderWithApp(<Issues projectId={3} />);
    await screen.findByText("a is undefined");
    // A regressed issue is labelled in words, not just by color.
    expect(screen.getByText("Regressed")).toBeTruthy();

    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "r" });

    await waitFor(() => expect(toast).toHaveBeenCalledWith("Resolved 1 issue"));
    const post = calls.find((c) => c.method === "POST")!;
    expect(post.body).toEqual({ ids: [11], status: "resolved" });
    expect(post.headers[CSRF_HEADER]).toBe("1");
    // It leaves the unresolved list.
    expect(screen.queryByText("too deep")).toBeNull();
  });

  test("typing r in the search box searches, it doesn't resolve", async () => {
    const { calls } = server();
    renderWithApp(<Issues projectId={3} />);
    await screen.findByText("a is undefined");
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Search issues"));
    await user.keyboard("r");
    expect(calls.some((c) => c.method === "POST")).toBe(false);
    await waitFor(() => expect(location.search).toBe("?q=r"));
  });

  test("a project that never received an error shows onboarding with its DSN", async () => {
    fakeServer({
      "GET /api/ui/projects/3": { ...project, counts: { unresolved: 0, resolved: 0, ignored: 0 } },
      "GET /api/ui/projects/3/issues": { issues: [], total: 0, counts: { unresolved: 0, resolved: 0, ignored: 0 } },
    });
    renderWithApp(<Issues projectId={3} />);
    expect(await screen.findByText("Connect your app")).toBeTruthy();
    await waitFor(() => expect(document.body.textContent).toContain('dsn: "http://pk_abc@localhost:8080/3"'));
  });
});

test("onboarding switches platform snippets", async () => {
  fakeServer({ "GET /api/ui/projects/3": project });
  render(<Onboarding projectId={3} />);
  await waitFor(() => expect(document.body.textContent).toContain("npm install @juju/web"));
  fireEvent.click(screen.getByRole("tab", { name: "Flutter" }));
  expect(document.body.textContent).toContain("flutter pub add juju_flutter");
  expect(document.body.textContent).toContain("dsn: 'http://pk_abc@localhost:8080/3'");
});

test("stack trace folds library frames and can show the original minified frame", () => {
  const frames = [
    { function: "render", filename: "src/App.tsx", lineno: 12, in_app: true, raw: { function: "a", filename: "app.min.js", lineno: 1, colno: 99 } },
    { function: "x", filename: "react-dom.js", in_app: false },
    { function: "y", filename: "react-dom.js", in_app: false },
  ];
  const { rerender } = render(<StackTrace frames={frames} showRaw={false} />);
  expect(screen.getByText("2 library frames · show")).toBeTruthy();
  expect(screen.queryByText(/app\.min\.js/)).toBeNull();
  fireEvent.click(screen.getByText("2 library frames · show"));
  expect(screen.getAllByText("react-dom.js")).toHaveLength(2);
  rerender(<StackTrace frames={frames} showRaw />);
  expect(screen.getByText(/app\.min\.js:1:99/)).toBeTruthy();
});

describe("issue page", () => {
  const ev = (n: number): EventPayload => ({
    event_id: `e${n}`,
    timestamp: new Date(Date.now() - n * 60_000).toISOString(),
    received_at: new Date().toISOString(),
    platform: "javascript",
    release: `web@1.0.${n}`,
    environment: "production",
    exception: { type: "TypeError", message: `boom ${n}` },
    sdk: { name: "juju.web", version: "0.1.0" },
    symbolication: n === 1 ? { status: "missing", notes: ["no source map for app.min.js"] } : undefined,
  });

  test("[ and ] move between events; Esc goes back to the list", async () => {
    history.replaceState(null, "", "/p/3/issues/10");
    fakeServer({
      "GET /api/ui/projects/3/issues/10": { ...row(10, "TypeError: boom"), fingerprint: "abcdef0123456789ff", environments: ["production"], releases: ["web@1.0.0"], commits: [], links: [], activity: [{ kind: "created", created_at: new Date().toISOString() }] },
      "GET /api/ui/projects/3/issues/10/events": { events: [ev(0), ev(1)] },
    });
    renderWithApp(<Issue projectId={3} issueId={10} />);
    expect(await screen.findByText(": boom 0")).toBeTruthy();
    expect(screen.queryByText(/^No source maps for/)).toBeNull();

    fireEvent.keyDown(window, { key: "]" });
    expect(await screen.findByText(": boom 1")).toBeTruthy();
    // The second event had no source map: say so, and what to do about it.
    expect(screen.getByText("No source maps for web@1.0.1")).toBeTruthy();
    expect(screen.getByText("no source map for app.min.js")).toBeTruthy();

    fireEvent.keyDown(window, { key: "[" });
    expect(await screen.findByText(": boom 0")).toBeTruthy();

    act(() => {
      fireEvent.keyDown(window, { key: "Escape" });
    });
    expect(location.pathname).toBe("/p/3/issues");
  });

  test("a missing issue says so instead of spinning", async () => {
    fakeServer({});
    renderWithApp(<Issue projectId={3} issueId={999} />);
    expect(await screen.findByText("This issue doesn't exist")).toBeTruthy();
  });
});
