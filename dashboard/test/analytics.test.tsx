import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { LineChart } from "../src/components/LineChart";
import { niceTicks, percent, seriesLabel } from "../src/lib/analytics";
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
  expect(document.body.textContent).toContain('juju.capture("checkout_started"');
  fireEvent.click(screen.getByRole("tab", { name: "Flutter" }));
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
