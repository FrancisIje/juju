// Formatting for the release health and performance pages.

import type { Period } from "./api";

export const periods: { id: Period; label: string }[] = [
  { id: "24h", label: "24 hours" },
  { id: "7d", label: "7 days" },
  { id: "14d", label: "14 days" },
  { id: "30d", label: "30 days" },
];

export function isPeriod(s: string | null): s is Period {
  return s === "24h" || s === "7d" || s === "14d" || s === "30d";
}

/** 0.99512 → "99.51%". Two decimals: at 99.9 % the difference that matters is in the second one. */
export function percent(rate: number | null | undefined): string {
  if (rate === null || rate === undefined) return "–";
  const p = rate * 100;
  if (p === 100) return "100%";
  return `${p >= 99 ? p.toFixed(2) : p.toFixed(1)}%`;
}

/** 0.4 → "400 ms", 1234 → "1.23 s", 75000 → "1m 15s". */
export function duration(ms: number | null | undefined): string {
  if (ms === null || ms === undefined || !Number.isFinite(ms)) return "–";
  if (ms < 1) return `${ms.toFixed(2)} ms`;
  if (ms < 1000) return `${Math.round(ms)} ms`;
  if (ms < 60_000) return `${(ms / 1000).toFixed(ms < 10_000 ? 2 : 1)} s`;
  const s = Math.round(ms / 1000);
  return `${Math.floor(s / 60)}m ${s % 60}s`;
}

/** Axis ticks: whole percents, "95%". */
export const percentTick = (rate: number) => `${Math.round(rate * 1000) / 10}%`;

/** Axis ticks: "0", "500 ms", "1.5 s". */
export function durationTick(ms: number): string {
  if (ms === 0) return "0";
  return ms < 1000 ? `${Math.round(ms)} ms` : `${Number((ms / 1000).toFixed(2))} s`;
}

/** Label for a chart bucket: hours for a 24 h period, days otherwise. */
export function bucketLabel(iso: string, period: Period): string {
  const d = new Date(iso);
  return period === "24h" ? d.toLocaleTimeString(undefined, { hour: "numeric" }) : d.toLocaleDateString(undefined, { month: "short", day: "numeric" });
}

/** What each op means, in plain words, for people new to tracing. */
export const opLabels: Record<string, string> = {
  pageload: "Page load",
  navigation: "Page change (single-page app)",
  "app.start": "App start",
  "screen.load": "Screen load",
  "http.client": "HTTP request",
  browser: "Browser: loading the page",
  "resource.script": "Scripts",
  "resource.css": "Stylesheets",
  "app.start.phase": "App start steps",
  custom: "Custom",
};
