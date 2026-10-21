import { fireEvent, screen, waitFor } from "@testing-library/react";
import userEvent from "@testing-library/user-event";
import type { ReleaseHealth, TransactionDetail, TransactionList, TransactionPayload } from "../src/lib/api";
import { duration, durationTick, percent, percentTick } from "../src/lib/perf";
import { niceTicks } from "../src/components/charts";
import { Performance } from "../src/pages/Performance";
import { Releases } from "../src/pages/Releases";
import { Transaction } from "../src/pages/Transaction";
import { fakeServer, renderWithApp } from "./helpers";

const day = (d: number) => new Date(Date.UTC(2026, 9, 14 + d)).toISOString();

const health: ReleaseHealth = {
  totals: { sessions: 200, crashed: 3, errored: 5, users: 40, crashed_users: 2, crash_free_sessions: 0.985, crash_free_users: 0.95 },
  releases: [
    { release: "shop@1.4.0", sessions: 150, crashed: 3, errored: 4, users: 30, crashed_users: 2, crash_free_sessions: 0.98, crash_free_users: 0.9333, adoption: 0.75, first_seen: day(0), last_seen: day(6) },
    { release: "shop@1.3.9", sessions: 50, crashed: 0, errored: 1, users: 10, crashed_users: 0, crash_free_sessions: 1, crash_free_users: 1, adoption: 0.25, first_seen: day(0), last_seen: day(3) },
  ],
  series: [0, 1, 2, 3, 4, 5, 6].map((d) => ({ time: day(d), sessions: d === 2 ? 0 : 30, crashed: d === 4 ? 3 : 0, errored: 0 })),
  environments: ["production", "staging"],
};

describe("releases", () => {
  beforeEach(() => history.replaceState(null, "", "/p/3/releases"));

  test("shows the crash-free rates in words and numbers, per release", async () => {
    fakeServer({ "GET /api/ui/projects/3/releases": health });
    renderWithApp(<Releases projectId={3} />);
    expect(await screen.findByText("98.5%")).toBeTruthy(); // totals
    expect(screen.getByText("197 of 200 sessions didn't crash")).toBeTruthy();
    expect(screen.getByText("shop@1.4.0")).toBeTruthy();
    expect(screen.getByText("98.0%")).toBeTruthy();
  });

  // Every value in the chart is readable without hovering: the table view
  // has them all, and a day with no sessions says so instead of showing 100 %.
  test("the chart has a table view, with empty days marked", async () => {
    fakeServer({ "GET /api/ui/projects/3/releases": health });
    renderWithApp(<Releases projectId={3} />);
    await screen.findByText("98.5%");
    fireEvent.click(screen.getByText("Show as a table"));
    const rows = screen.getAllByRole("row").filter((r) => r.closest("details"));
    expect(rows).toHaveLength(8); // header + 7 days
    expect(rows[3]!.textContent).toContain("–"); // the day with no sessions
    expect(rows[5]!.textContent).toContain("90.0%"); // 27 of 30 crash-free
  });

  test("the period and environment live in the URL", async () => {
    const { calls } = fakeServer({ "GET /api/ui/projects/3/releases": health });
    renderWithApp(<Releases projectId={3} />);
    await screen.findByText("98.5%");
    fireEvent.click(screen.getByRole("button", { name: "24h" }));
    await waitFor(() => expect(location.search).toBe("?period=24h"));
    fireEvent.change(screen.getByLabelText("Environment"), { target: { value: "staging" } });
    await waitFor(() => expect(location.search).toBe("?period=24h&environment=staging"));
    await waitFor(() => expect(calls.at(-1)!.path).toBe("/api/ui/projects/3/releases?period=24h&environment=staging"));
  });

  test("with no sessions, says how to get them instead of showing 100 %", async () => {
    fakeServer({ "GET /api/ui/projects/3/releases": { ...health, totals: { ...health.totals, sessions: 0, crashed: 0, crash_free_sessions: null, crash_free_users: null }, releases: [], series: [] } });
    renderWithApp(<Releases projectId={3} />);
    expect(await screen.findByText("No sessions yet")).toBeTruthy();
    expect(screen.queryByText("100%")).toBeNull();
  });

  test("a server error is shown in words with a retry", async () => {
    vi.stubGlobal("fetch", vi.fn(async () => new Response(JSON.stringify({ error: "something went wrong on the server; try again" }), { status: 500 })));
    renderWithApp(<Releases projectId={3} />);
    expect((await screen.findByRole("alert")).textContent).toContain("Something went wrong on the server");
    expect(screen.getByRole("button", { name: "Retry" })).toBeTruthy();
  });
});

const list: TransactionList = {
  transactions: [
    { name: "/checkout", op: "pageload", count: 120, p50: 840, p75: 1200, p95: 2400, failed: 2, last_seen: day(6) },
    { name: "app.start", op: "app.start", count: 40, p50: 1900, p75: 2200, p95: 3100, failed: 0, last_seen: day(6) },
  ],
  ops: ["app.start", "pageload"],
  environments: ["production"],
};

describe("performance list", () => {
  beforeEach(() => history.replaceState(null, "", "/p/3/performance"));

  test("lists transactions with plain-language kinds and percentiles", async () => {
    fakeServer({ "GET /api/ui/projects/3/transactions": list });
    renderWithApp(<Performance projectId={3} />);
    expect(await screen.findByText("/checkout")).toBeTruthy();
    expect(screen.getAllByText("Page load").length).toBeGreaterThan(0);
    expect(screen.getByText("2.40 s")).toBeTruthy();
  });

  test("j then Enter opens the second transaction, keeping the period", async () => {
    history.replaceState(null, "", "/p/3/performance?period=24h");
    fakeServer({ "GET /api/ui/projects/3/transactions": list });
    renderWithApp(<Performance projectId={3} />);
    await screen.findByText("/checkout");
    fireEvent.keyDown(window, { key: "j" });
    fireEvent.keyDown(window, { key: "Enter" });
    expect(location.pathname).toBe("/p/3/performance/transaction");
    expect(Object.fromEntries(new URLSearchParams(location.search))).toEqual({ name: "app.start", op: "app.start", period: "24h" });
  });

  test("shortcuts don't fire while choosing a filter", async () => {
    fakeServer({ "GET /api/ui/projects/3/transactions": list });
    renderWithApp(<Performance projectId={3} />);
    await screen.findByText("/checkout");
    const user = userEvent.setup();
    await user.click(screen.getByLabelText("Sort by"));
    await user.keyboard("{Enter}");
    expect(location.pathname).toBe("/p/3/performance");
  });

  test("an empty project explains what gets timed", async () => {
    fakeServer({ "GET /api/ui/projects/3/transactions": { transactions: [], ops: [], environments: [] } });
    renderWithApp(<Performance projectId={3} />);
    expect(await screen.findByText("Nothing timed yet")).toBeTruthy();
  });
});

describe("transaction page", () => {
  const detail: TransactionDetail = {
    summary: list.transactions[0]!,
    series: [0, 1].map((d) => ({ time: day(d), count: 60, p50: 800 + d * 80, p95: 2300 + d * 200 })),
    releases: [{ release: "web@2.0", count: 120, p50: 840, p95: 2400 }],
    span_ops: [{ op: "http.client", count: 240, p50: 300, p95: 900, per_transaction: 610 }],
    samples: [
      { event_id: "11111111-1111-4111-8111-111111111111", started_at: day(6), duration_ms: 3100, status: "ok", release: "web@2.0", environment: "production", platform: "javascript", spans: 2 },
      { event_id: "22222222-2222-4222-8222-222222222222", started_at: day(5), duration_ms: 2900, status: "deadline_exceeded", release: "web@2.0", environment: "production", platform: "javascript", spans: 1 },
    ],
  };
  const payload = (id: string): TransactionPayload => ({
    event_id: id,
    timestamp: day(6),
    platform: "javascript",
    environment: "production",
    transaction: {
      name: "/checkout",
      op: "pageload",
      status: "ok",
      start_timestamp: day(6),
      duration_ms: 3100,
      measurements: { ttfb: 120, lcp: 1800 },
      spans: [
        { span_id: "a", op: "browser", description: "Waiting for the server", start_ms: 10, duration_ms: 110 },
        { span_id: "b", op: "http.client", description: "GET /api/cart", status: "error", start_ms: 900, duration_ms: 2100, data: { status_code: 503 } },
      ],
    },
  });

  beforeEach(() => history.replaceState(null, "", "/p/3/performance/transaction?name=%2Fcheckout&op=pageload"));

  test("shows the slowest sample's spans, with a failed request labelled in words", async () => {
    const { calls } = fakeServer({
      "GET /api/ui/projects/3/transactions/summary": detail,
      "GET /api/ui/projects/3/transactions/events/11111111-1111-4111-8111-111111111111": payload("11111111-1111-4111-8111-111111111111"),
      "GET /api/ui/projects/3/transactions/events/22222222-2222-4222-8222-222222222222": payload("22222222-2222-4222-8222-222222222222"),
    });
    renderWithApp(<Transaction projectId={3} />);
    expect(await screen.findByText("GET /api/cart")).toBeTruthy();
    expect(screen.getByText("Server error")).toBeTruthy();
    expect(screen.getByText("Main content on screen")).toBeTruthy(); // lcp, in plain words
    expect(calls[0]!.path).toContain("name=%2Fcheckout");

    // ] moves to the next sample.
    fireEvent.keyDown(window, { key: "]" });
    await waitFor(() => expect(calls.some((c) => c.path.endsWith("22222222-2222-4222-8222-222222222222"))).toBe(true));
    // The timed-out sample is marked in words.
    expect(screen.getByText("Timed out")).toBeTruthy();
  });

  test("Esc goes back to the list, keeping the period", async () => {
    history.replaceState(null, "", "/p/3/performance/transaction?name=%2Fcheckout&op=pageload&period=14d");
    fakeServer({ "GET /api/ui/projects/3/transactions/summary": detail });
    renderWithApp(<Transaction projectId={3} />);
    await screen.findByText("Typical (p50)");
    fireEvent.keyDown(window, { key: "Escape" });
    expect(location.pathname + location.search).toBe("/p/3/performance?period=14d");
  });

  test("the chart can be read from the keyboard", async () => {
    fakeServer({ "GET /api/ui/projects/3/transactions/summary": detail });
    renderWithApp(<Transaction projectId={3} />);
    const chart = await screen.findByRole("img", { name: /Duration per day/ });
    chart.focus();
    fireEvent.keyDown(chart, { key: "ArrowRight" });
    const tip = await screen.findByRole("status");
    expect(tip.textContent).toContain("2.30 s"); // p95 of the first day
    expect(tip.textContent).toContain("800 ms");
  });
});

describe("formatting", () => {
  test.each([
    [0.25, "0.25 ms"],
    [340, "340 ms"],
    [1234, "1.23 s"],
    [75_000, "1m 15s"],
  ])("duration(%s) = %s", (ms, want) => expect(duration(ms)).toBe(want));

  test("percent keeps the digit that matters near 100 %", () => {
    expect(percent(0.99512)).toBe("99.51%");
    expect(percent(0.9)).toBe("90.0%");
    expect(percent(1)).toBe("100%");
    expect(percent(null)).toBe("–");
  });

  test("axis labels read as round numbers, with no false precision", () => {
    expect([1, 0.95, 0.9].map(percentTick)).toEqual(["100%", "95%", "90%"]);
    expect(percentTick(0.975)).toBe("97.5%");
    expect([0, 500, 1000, 1500].map(durationTick)).toEqual(["0", "500 ms", "1 s", "1.5 s"]);
  });

  test("axis ticks are round numbers", () => {
    expect(niceTicks(2400)).toEqual([0, 1000, 2000, 3000]);
    expect(niceTicks(0)).toEqual([0, 1]);
  });
});
