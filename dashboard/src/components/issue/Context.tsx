import type { EventPayload } from "../../lib/api";

/** Nested context ({device: {model: "Pixel"}}) as flat rows: device.model = Pixel. */
export function flatten(obj: Record<string, unknown>, prefix = ""): [string, string][] {
  const out: [string, string][] = [];
  for (const [k, v] of Object.entries(obj)) {
    const key = prefix ? `${prefix}.${k}` : k;
    if (v && typeof v === "object" && !Array.isArray(v)) out.push(...flatten(v as Record<string, unknown>, key));
    else out.push([key, typeof v === "string" ? v : JSON.stringify(v)]);
  }
  return out;
}

function Section({ title, rows }: { title: string; rows: [string, string][] }) {
  if (rows.length === 0) return null;
  return (
    <section style={{ display: "grid", gap: "var(--space-2)" }}>
      <h3 className="muted" style={{ padding: "0 var(--space-3)" }}>
        {title}
      </h3>
      <dl className="kv">
        {rows.map(([k, v]) => [<dt key={`k-${k}`}>{k}</dt>, <dd key={`v-${k}`}>{v}</dd>])}
      </dl>
    </section>
  );
}

// Who, where and on what: the facts that tell you whether a bug hits one
// browser, one release or one user.
export function Context({ event }: { event: EventPayload }) {
  const basics: [string, string][] = [
    ["release", event.release ?? "(none)"],
    ["environment", event.environment],
    ["platform", event.platform],
    ["sdk", `${event.sdk.name} ${event.sdk.version}`.trim()],
    ["event id", event.event_id],
  ];
  return (
    <div style={{ display: "grid", gap: "var(--space-5)", padding: "var(--space-4) 0" }}>
      <Section title="Event" rows={basics} />
      <Section title="User" rows={flatten(event.user ?? {})} />
      <Section title="Tags" rows={flatten(event.tags ?? {})} />
      <Section title="Device and app" rows={flatten(event.context ?? {})} />
      {event.repairs?.length ? <Section title="Fixed by the server on arrival" rows={event.repairs.map((r, i) => [String(i + 1), r])} /> : null}
    </div>
  );
}
