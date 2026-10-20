import { useState } from "react";
import type { Frame } from "../../lib/api";
import { groupFrames, location } from "../../lib/frames";

// The stack trace, most recent call first. Your own code (in-app frames) is
// marked with a violet edge; runs of library frames fold into one line so
// the frames you can actually fix are what you see first.
export function StackTrace({ frames, showRaw }: { frames: Frame[]; showRaw: boolean }) {
  const [showAll, setShowAll] = useState(false);
  const [opened, setOpened] = useState<Set<number>>(new Set());
  if (frames.length === 0) return <p className="muted" style={{ padding: "var(--space-4)" }}>This error was sent without a stack trace.</p>;

  const groups = groupFrames(frames, showAll);
  const hasLibrary = frames.some((f) => !f.in_app) && frames.some((f) => f.in_app);
  return (
    <div>
      {hasLibrary && (
        <div className="toolbar" style={{ padding: "var(--space-2) var(--space-3)", borderBottom: "1px solid var(--line)" }}>
          <label className="switch" style={{ fontSize: "var(--text-xs)" }}>
            <input type="checkbox" checked={showAll} onChange={(e) => setShowAll(e.target.checked)} />
            Show library frames
          </label>
        </div>
      )}
      <ol className="frames" style={{ margin: 0, padding: 0, listStyle: "none" }} aria-label="Stack frames, most recent first">
        {groups.map((g) =>
          g.kind === "frame" ? (
            <FrameRow key={g.index} frame={g.frame} showRaw={showRaw} />
          ) : opened.has(g.start) ? (
            g.frames.map((f, i) => <FrameRow key={g.start + i} frame={f} showRaw={showRaw} />)
          ) : (
            <li key={`c-${g.start}`}>
              <button className="collapsed-frames" onClick={() => setOpened(new Set(opened).add(g.start))}>
                {g.frames.length} library frames · show
              </button>
            </li>
          ),
        )}
      </ol>
    </div>
  );
}

function FrameRow({ frame, showRaw }: { frame: Frame; showRaw: boolean }) {
  const loc = location(frame);
  return (
    <li className={`frame${frame.in_app ? " app" : ""}`}>
      <span className="fn">{frame.function || "<anonymous>"}</span>
      {loc && <span className="loc">{loc}</span>}
      {showRaw && frame.raw && (
        <span className="raw">
          was {frame.raw.function ? `${frame.raw.function} · ` : ""}
          {location(frame.raw)}
        </span>
      )}
    </li>
  );
}
