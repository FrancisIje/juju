import type { Frame } from "./api";

export type FrameGroup = { kind: "frame"; frame: Frame; index: number } | { kind: "collapsed"; frames: Frame[]; start: number };

/**
 * Groups runs of library (not in-app) frames so the stack shows your code
 * first, with "12 library frames" in between that expand on click. A trace
 * with no in-app frames at all is shown in full: hiding everything would
 * hide the only clues.
 */
export function groupFrames(frames: Frame[], showAll: boolean): FrameGroup[] {
  if (showAll || !frames.some((f) => f.in_app)) {
    return frames.map((frame, index) => ({ kind: "frame", frame, index }));
  }
  const out: FrameGroup[] = [];
  let run: Frame[] = [];
  let runStart = 0;
  const flush = () => {
    if (run.length === 1) out.push({ kind: "frame", frame: run[0]!, index: runStart });
    else if (run.length > 1) out.push({ kind: "collapsed", frames: run, start: runStart });
    run = [];
  };
  frames.forEach((f, i) => {
    if (f.in_app) {
      flush();
      out.push({ kind: "frame", frame: f, index: i });
    } else {
      if (run.length === 0) runStart = i;
      run.push(f);
    }
  });
  flush();
  return out;
}

export function location(f: { filename?: string; lineno?: number; colno?: number }): string {
  if (!f.filename) return "";
  return f.filename + (f.lineno ? `:${f.lineno}` : "") + (f.lineno && f.colno ? `:${f.colno}` : "");
}

/** "TypeError: Cannot read…" → ["TypeError", "Cannot read…"] */
export function splitTitle(title: string): [string, string] {
  const i = title.indexOf(": ");
  return i > 0 && i < 60 ? [title.slice(0, i), title.slice(i + 2)] : [title, ""];
}
