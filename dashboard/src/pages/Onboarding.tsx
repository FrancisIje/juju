import { useState } from "react";
import { Code } from "../components/CopyButton";
import type { ProjectInfo } from "../lib/api";
import { platforms, snippet, type Platform } from "../lib/snippets";
import { useApi } from "../lib/useApi";

// Shown instead of an empty issue list until the first error arrives: the
// three steps to connect an app, with this project's DSN already filled in.
export function Onboarding({ projectId }: { projectId: number }) {
  const { data } = useApi<{ project: ProjectInfo }>(`/api/ui/projects/${projectId}`);
  const [platform, setPlatform] = useState<Platform>("web");
  const dsn = data?.project.dsn ?? "…";
  const s = snippet(platform, dsn);
  return (
    <section className="panel onboard" aria-labelledby="onboard-title">
      <div style={{ display: "grid", gap: 4 }}>
        <span className="chip brand" style={{ justifySelf: "start" }}>
          Waiting for the first error
        </span>
        <h2 id="onboard-title">Connect your app</h2>
        <p className="muted">Errors appear here a few seconds after they happen. This page updates by itself.</p>
      </div>
      <div className="tabs" role="tablist" aria-label="Platform">
        {platforms.map((p) => (
          <button key={p.id} role="tab" aria-selected={platform === p.id} onClick={() => setPlatform(p.id)}>
            {p.label}
          </button>
        ))}
      </div>
      <ol className="steps-list">
        {s.install && (
          <li>
            <div style={{ display: "grid", gap: 8 }}>
              <span>Install the SDK</span>
              <Code>{s.install}</Code>
            </div>
          </li>
        )}
        <li>
          <div style={{ display: "grid", gap: 8 }}>
            <span>{platform === "curl" ? "Send a test error from your terminal" : "Start it as early as possible when your app launches"}</span>
            <Code>{s.code}</Code>
          </div>
        </li>
        <li>
          <span className="muted">
            {platform === "curl" ? "Run it, then watch this page." : "Trigger an error (or wait for a real one). It shows up here with its stack trace and what led to it."}
          </span>
        </li>
      </ol>
      <p className="faint" style={{ fontSize: "var(--text-xs)" }}>
        Your DSN is safe to ship inside apps: it can only send errors, not read them. Find it again under Settings → Setup.
      </p>
    </section>
  );
}
