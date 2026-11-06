import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LineChart } from "../src/components/LineChart";
import { joinSteps, niceTicks, percent, seriesLabel, splitSteps } from "../src/lib/analytics";
import { Analytics } from "../src/pages/Analytics";
import { fakeServer, renderWithApp } from "./helpers";

const days = ["2026-10-14T00:00:00Z", "2026-10-15T00:00:00Z", "2026-10-16T00:00:00Z"];
const summary = { enabled: true, events: [{ name: "checkout_started", count: 9, last_seen: 0 }, { name: "checkout_completed", count: 2, last_seen: 0 }, { name: "$identify", count: 1, last_seen: 0 }] };

beforeEach(() => history.replaceState(null, "", "/p/3/analytics/trends"));

test("labels people read instead of internal names", () => {
  expect(seriesLabel("lib", "juju.react-native")).toBe("React Native");
  expect(seriesLabel("lib", "juju.web")).toBe("Web");
  expect(seriesLabel("os", "")).toBe("(none)");
  expect(seriesLabel("", "")).toBe("All");
  expect(percent(1, 3)).toBe("33%");
  expect(percent(1, 30)).toBe("3.3%");
  expect(percent(1, 0)).toBe("–");
  expect(niceTicks(87)).toEqual([0, 25, 50, 75, 100]);
  expect(niceTicks(7)).toEqual([0, 2, 4, 6, 8]);
  expect(niceTicks(0)).toEqual([0, 1]);
  // Counts are whole numbers: no "0.25 events" on the axis.
  expect(niceTicks(1)).toEqual([0, 1]);
  expect(niceTicks(9)).toEqual([0, 5, 10]);
  // Shares below 1 still get steps that fit.
  expect(niceTicks(0.5)).toEqual([0, 0.2, 0.4, 0.6]);
});

test("analytics off: says what's needed instead of an empty chart", async () => {
  fakeServer({ "GET /api/ui/projects/3/analytics": { enabled: false, reason: "needs ClickHouse", events: [] } });
  renderWithApp(<Analytics projectId={3} tab="trends" />);
  expect(await screen.findByRole("heading", { name: "Analytics needs ClickHouse" })).toBeTruthy();
  expect(document.body.textContent).toContain("JUJU_CLICKHOUSE_URL");
});

test("no events yet: how to send the first one, per platform", async () => {
  fakeServer({
    "GET /api/ui/projects/3/analytics": { enabled: true, events: [] },
    "GET /api/ui/projects/3": { project: { dsn: "http://pk_a@localhost:8080/3" } },
  });
  renderWithApp(<Analytics projectId={3} tab="trends" />);
  expect(await screen.findByRole("heading", { name: "Track what people do" })).toBeTruthy();
  // Each one works when pasted: the SDK started with this project's DSN.
  await waitFor(() => expect(document.body.textContent).toContain('dsn: "http://pk_a@localhost:8080/3"'));
  expect(document.body.textContent).toContain('juju.capture("checkout_started"');
  fireEvent.click(screen.getByRole("tab", { name: "React Native" }));
  expect(document.body.textContent).toContain('dsn: "http://pk_a@localhost:8080/3"');
  fireEvent.click(screen.getByRole("tab", { name: "Flutter" }));
  expect(document.body.textContent).toContain("dsn: 'http://pk_a@localhost:8080/3'");
  expect(document.body.textContent).toContain("Juju.capture('checkout_started'");
});

test("trends: by platform, with readable labels, and the choice kept in the URL", async () => {
  const { calls } = fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/properties": { properties: ["plan"] },
    "GET /api/ui/projects/3/analytics/trend": (_: unknown) => ({
      buckets: days,
      series: [
        { label: "juju.web", values: [2, 0, 2], total: 4 },
        { label: "juju.flutter", values: [0, 2, 0], total: 2 },
      ],
      total: 6,
      hidden: 0,
    }),
  });
  renderWithApp(<Analytics projectId={3} tab="trends" />);
  // "$identify" is internal: not offered as an event.
  const eventSelect = (await screen.findByLabelText("Event")) as HTMLSelectElement;
  expect([...eventSelect.options].map((o) => o.value)).toEqual(["checkout_started", "checkout_completed"]);

  fireEvent.change(screen.getByLabelText("Break down by"), { target: { value: "lib" } });
  await waitFor(() => expect(location.search).toBe("?breakdown=lib"));
  await waitFor(() => expect(calls.some((c) => c.path.includes("breakdown=lib"))).toBe(true));
  expect(await screen.findByRole("cell", { name: /^Web$/ })).toBeTruthy();
  expect(screen.getByRole("cell", { name: /^Flutter$/ })).toBeTruthy();
  expect(screen.getByText("67%")).toBeTruthy(); // Web: 4 of 6
  // Properties are offered as breakdowns too.
  expect(screen.getByRole("option", { name: "plan" })).toBeTruthy();

  fireEvent.click(screen.getByRole("button", { name: "People" }));
  await waitFor(() => expect(location.search).toContain("math=people"));
});

test("trends: the headline is the server's total, not the lines added up", async () => {
  // Someone active on two days is one person, though each day's line has them.
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/properties": { properties: [] },
    "GET /api/ui/projects/3/analytics/trend": { buckets: days, series: [{ label: "", values: [1, 2, 0], total: 2 }], total: 2, hidden: 0 },
  });
  history.replaceState(null, "", "/p/3/analytics/trends?math=people");
  renderWithApp(<Analytics projectId={3} tab="trends" />);
  await waitFor(() => expect(document.querySelector(".panel .mono")!.textContent).toBe("2"));
  expect(screen.getByText(/people did/)).toBeTruthy();
});

test("trends: values the chart hides still count in the total and shares", async () => {
  const series = Array.from({ length: 10 }, (_, i) => ({ label: `val${i}`, values: [1, 0, 0], total: 1 }));
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/properties": { properties: ["v"] },
    "GET /api/ui/projects/3/analytics/trend": { buckets: days, series, total: 12, hidden: 2 },
  });
  history.replaceState(null, "", "/p/3/analytics/trends?breakdown=property:v");
  renderWithApp(<Analytics projectId={3} tab="trends" />);
  expect(await screen.findByRole("cell", { name: /val0/ })).toBeTruthy();
  expect(document.querySelector(".panel .mono")!.textContent).toBe("12");
  expect(screen.getAllByText("8.3%")).toHaveLength(10); // 1 of 12, not 1 of 10
});

// Refresh must change what's on screen: the fake answers differently each
// time, as the real server does once new events arrive.
test.each([
  ["trends", "trend", (n: number) => ({ buckets: days, series: [{ label: "", values: [n, 0, 0], total: n }], total: n, hidden: 0 }), "7 ", "8 "],
  ["funnels", "funnel", (n: number) => ({ steps: [{ event: "checkout_started", people: n }, { event: "checkout_completed", people: 1 }] }), "7 people", "8 people"],
  ["retention", "retention", (n: number) => ({ periods: 1, cohorts: [{ day: "2026-01-05T00:00:00Z", people: n, returned: [1] }] }), "7", "8"],
] as const)("Refresh reloads the %s tab", async (tab, route, answer, before, after) => {
  let n = 7;
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/properties": { properties: [] },
    [`GET /api/ui/projects/3/analytics/${route}`]: () => answer(n),
  });
  history.replaceState(null, "", `/p/3/analytics/${tab}`);
  renderWithApp(<Analytics projectId={3} tab={tab} />);
  const shown = () => document.querySelector(".funnel-step, section[aria-busy] .mono, tbody td.num")?.textContent ?? "";
  await waitFor(() => expect(shown()).toContain(before.trim()));
  n = 8;
  fireEvent.click(screen.getByRole("button", { name: "Refresh" }));
  await waitFor(() => expect(shown()).toContain(after.trim()));
});

test("funnels: a comma in an event name stays part of the name", async () => {
  const withComma = { enabled: true, events: [{ name: "plan,changed", count: 1, last_seen: 0 }, { name: "paid", count: 1, last_seen: 0 }, { name: "left", count: 1, last_seen: 0 }] };
  const { calls } = fakeServer({
    "GET /api/ui/projects/3/analytics": withComma,
    "GET /api/ui/projects/3/analytics/funnel": { steps: [{ event: "plan,changed", people: 1 }, { event: "paid", people: 1 }] },
  });
  history.replaceState(null, "", "/p/3/analytics/funnels?steps=left,paid");
  renderWithApp(<Analytics projectId={3} tab="funnels" />);
  const first = (await screen.findByLabelText("Step 1")) as HTMLSelectElement;
  fireEvent.change(first, { target: { value: "plan,changed" } });
  await waitFor(() => expect((screen.getByLabelText("Step 1") as HTMLSelectElement).value).toBe("plan,changed"));
  expect((screen.getByLabelText("Step 2") as HTMLSelectElement).value).toBe("paid");
  expect(screen.queryByLabelText("Step 3")).toBeNull();
  await waitFor(() => expect(calls.some((c) => c.path.includes("step=plan%2Cchanged&step=paid"))).toBe(true));
});

test("funnel links: names with commas round-trip, and old saved links still work", () => {
  expect(splitSteps(joinSteps(["plan,changed", "100%", "paid"]))).toEqual(["plan,changed", "100%", "paid"]);
  expect(joinSteps(["signed_up", "paid"])).toBe("signed_up,paid");
  expect(splitSteps("signed_up,paid")).toEqual(["signed_up", "paid"]);
  expect(splitSteps("50%off,paid")).toEqual(["50%off", "paid"]);
});

test("retention: the picker shows the event the page is answering about", async () => {
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/retention": { periods: 7, cohorts: [] },
  });
  history.replaceState(null, "", "/p/3/analytics/retention?start=old_event");
  renderWithApp(<Analytics projectId={3} tab="retention" />);
  await screen.findByRole("heading", { name: "Nobody did old_event in this range" });
  expect((screen.getByLabelText("Starting event") as HTMLSelectElement).value).toBe("old_event");
});

test("funnels: conversion from the first step and drop-off from the one before", async () => {
  history.replaceState(null, "", "/p/3/analytics/funnels");
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/funnel": { steps: [{ event: "checkout_started", people: 4 }, { event: "checkout_completed", people: 1 }] },
  });
  renderWithApp(<Analytics projectId={3} tab="funnels" />);
  expect(await screen.findByText("1 person · 25%")).toBeTruthy();
  expect(screen.getByText("25% of step 1, 3 dropped off")).toBeTruthy();
});

test("retention: share per day, and days that haven't happened yet aren't 0%", async () => {
  history.replaceState(null, "", "/p/3/analytics/retention");
  const today = new Date();
  today.setUTCHours(0, 0, 0, 0);
  fakeServer({
    "GET /api/ui/projects/3/analytics": summary,
    "GET /api/ui/projects/3/analytics/retention": { periods: 3, cohorts: [{ day: new Date(today.getTime() - 5 * 864e5).toISOString(), people: 4, returned: [2, 1, 0] }, { day: today.toISOString(), people: 2, returned: [0, 0, 0] }] },
  });
  renderWithApp(<Analytics projectId={3} tab="retention" />);
  expect(await screen.findByText("50%")).toBeTruthy();
  expect(screen.getByText("25%")).toBeTruthy();
  // Today's cohort: day 1 is tomorrow, so it's a dot, not "0%".
  expect(screen.getAllByTitle("Hasn't happened yet")).toHaveLength(3);
});

test("the chart describes itself, and arrow keys read values at each point", () => {
  render(<LineChart buckets={days} series={[{ label: "Web", values: [2, 5, 1] }]} interval="day" />);
  const chart = screen.getByRole("img");
  expect(chart.getAttribute("aria-label")).toContain("8 in total");
  chart.focus();
  fireEvent.keyDown(chart, { key: "ArrowRight" });
  fireEvent.keyDown(chart, { key: "ArrowRight" });
  expect(chart.textContent).toContain("5");
});

test("x-axis labels never overlap, even on a narrow chart", () => {
  // The chart on a 390px phone is about 350px wide. (The test DOM measures 0.)
  vi.spyOn(HTMLElement.prototype, "clientWidth", "get").mockReturnValue(350);
  const eight = Array.from({ length: 8 }, (_, i) => `2026-10-${String(13 + i).padStart(2, "0")}T00:00:00Z`);
  const { container } = render(<LineChart buckets={eight} series={[{ label: "Web", values: [0, 0, 0, 0, 0, 0, 1, 3] }]} interval="day" />);
  const xs = [...container.querySelectorAll("text.chart-axis[text-anchor]")]
    .filter((t) => t.getAttribute("y") === "252") // the x-axis row: height 260 - 8
    .map((t) => Number(t.getAttribute("x")));
  for (let i = 1; i < xs.length; i++) expect(xs[i]! - xs[i - 1]!).toBeGreaterThanOrEqual(56);
  expect(xs.length).toBeGreaterThan(1);
});
