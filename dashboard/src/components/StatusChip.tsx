import type { Status } from "../lib/api";

const labels: Record<Status, string> = { unresolved: "Unresolved", resolved: "Resolved", ignored: "Ignored" };

// Status is always a word plus a color, never color alone, so it reads for
// everyone, including people who can't tell red from green.
export function StatusChip({ status, regressed }: { status: Status; regressed?: boolean }) {
  if (regressed && status === "unresolved") {
    return (
      <span className="chip regressed">
        <span className="dot" />
        Regressed
      </span>
    );
  }
  return (
    <span className={`chip ${status}`}>
      <span className="dot" />
      {labels[status]}
    </span>
  );
}

export function PlatformChip({ platform }: { platform: string }) {
  if (!platform) return null;
  const label = platform === "javascript" ? "JavaScript" : platform === "dart" ? "Dart" : platform;
  return <span className="chip">{label}</span>;
}
