// Shapes returned by /api/ui/projects/:id/analytics, and the labels people
// read instead of internal names. Mirrors server/internal/analytics.

export interface EventName {
  name: string;
  count: number;
  last_seen: number; // Unix seconds
}

export interface Summary {
  enabled: boolean;
  reason?: string;
  events: EventName[];
}

export interface Series {
  label: string;
  values: number[];
  total: number;
}

export interface Trend {
  buckets: string[];
  series: Series[];
  /** The whole range: every event or different person, hidden values included. */
  total: number;
  hidden: number;
}

export interface FunnelStep {
  event: string;
  people: number;
}

export interface Cohort {
  day: string;
  people: number;
  returned: number[];
}

export type Interval = "hour" | "day" | "week";

/** Time ranges offered, each with the interval that suits it. */
export const ranges: { id: string; label: string; days: number; interval: Interval }[] = [
  { id: "24h", label: "Last 24 hours", days: 1, interval: "hour" },
  { id: "7d", label: "Last 7 days", days: 7, interval: "day" },
  { id: "30d", label: "Last 30 days", days: 30, interval: "day" },
  { id: "90d", label: "Last 90 days", days: 90, interval: "week" },
];

/** What a trend can be split by. Event properties are added at runtime. */
export const breakdowns: { id: string; label: string }[] = [
  { id: "", label: "No breakdown" },
  { id: "lib", label: "Platform" },
  { id: "os", label: "Operating system" },
  { id: "browser", label: "Browser" },
  { id: "release", label: "Release" },
  { id: "environment", label: "Environment" },
];

const libNames: Record<string, string> = {
  "juju.web": "Web",
  "juju.react-native": "React Native",
  "juju.flutter": "Flutter",
};

/** A breakdown value as people read it: "juju.web" → "Web", "" → "(none)". */
export function seriesLabel(breakdown: string, label: string): string {
  if (breakdown === "") return "All";
  if (label === "") return "(none)";
  if (breakdown === "lib") return libNames[label] ?? label;
  return label;
}

/** A bucket's time, short enough for an axis: "Oct 20", or "14:00" for hours. */
export function bucketLabel(iso: string, interval: Interval): string {
  const d = new Date(iso);
  if (interval === "hour") return d.toLocaleTimeString(undefined, { hour: "2-digit", minute: "2-digit", timeZone: "UTC" });
  return d.toLocaleDateString(undefined, { month: "short", day: "numeric", timeZone: "UTC" });
}

/** "42%", from part of a whole; "–" when there's nothing to divide by. */
export function percent(part: number, whole: number): string {
  if (!whole) return "–";
  const p = (part / whole) * 100;
  return `${p < 10 && p > 0 ? p.toFixed(1) : Math.round(p)}%`;
}

/** Axis ticks: about `count` round numbers from 0 to at least max. */
export function niceTicks(max: number, count = 4): number[] {
  if (max <= 0) return [0, 1];
  const rough = max / count;
  const mag = 10 ** Math.floor(Math.log10(rough));
  // Counts are whole numbers, so from 1 up the steps are too: no "0.25 events".
  const nice = [1, 2, 2.5, 5, 10].map((m) => m * mag).filter((s) => max < 1 || Number.isInteger(s));
  const step = nice.find((s) => s >= rough) ?? rough;
  const top = Math.ceil(max / step) * step;
  const ticks: number[] = [];
  for (let v = 0; v <= top + step / 2; v += step) ticks.push(Math.round(v * 1e6) / 1e6);
  return ticks;
}

/**
 * Funnel steps in one URL value, so a funnel can be bookmarked. A comma inside
 * an event name is written %2C, or it would read as two steps. Plain names look
 * the same as before, so links people already saved keep working.
 */
export function joinSteps(steps: string[]): string {
  return steps.map((s) => s.replaceAll("%", "%25").replaceAll(",", "%2C")).join(",");
}

export function splitSteps(value: string): string[] {
  return value
    .split(",")
    .filter(Boolean)
    .map((s) => s.replaceAll("%2C", ",").replaceAll("%25", "%"));
}
