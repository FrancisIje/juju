import type { Breadcrumb } from "../../lib/api";

function clock(iso?: string): string {
  if (!iso) return "";
  const d = new Date(iso);
  return Number.isNaN(d.getTime()) ? "" : d.toLocaleTimeString(undefined, { hour12: false });
}

function describe(b: Breadcrumb): string {
  if (b.message) return b.message;
  // Network and navigation crumbs carry their details in data.
  const d = b.data ?? {};
  if (d.url) return [d.method, d.url, d.status_code].filter(Boolean).join(" ");
  if (d.to) return `${d.from ?? ""} → ${d.to}`;
  return Object.keys(d).length ? JSON.stringify(d) : "";
}

// What happened just before the error, oldest first, ending with the error
// itself: usually the fastest way to see how someone got into the bad state.
export function Breadcrumbs({ crumbs, error, timestamp }: { crumbs: Breadcrumb[]; error: string; timestamp: string }) {
  if (crumbs.length === 0) {
    return <p className="muted" style={{ padding: "var(--space-4)" }}>No breadcrumbs. The SDKs record clicks, navigation, network calls and console output on their own; add your own with addBreadcrumb.</p>;
  }
  return (
    <ol className="crumbs" style={{ margin: 0, padding: 0, listStyle: "none" }}>
      {crumbs.map((b, i) => (
        <li className="crumb" key={i}>
          <span className="t">{clock(b.timestamp)}</span>
          <span className="chip" style={{ justifySelf: "start" }}>
            {b.type || "default"}
          </span>
          <span style={{ overflowWrap: "anywhere" }}>{describe(b)}</span>
        </li>
      ))}
      <li className="crumb error">
        <span className="t">{clock(timestamp)}</span>
        <span className="chip unresolved" style={{ justifySelf: "start" }}>
          error
        </span>
        <span style={{ overflowWrap: "anywhere" }}>{error}</span>
      </li>
    </ol>
  );
}
