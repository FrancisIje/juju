import type { ExceptionData, Frame } from "./types";

// JavaScript engines print stack traces as plain text, in two dialects:
//
//   Chrome, Edge, Node (V8):     "    at render (https://shop.com/app.js:1:4821)"
//                                "    at https://shop.com/app.js:1:4821"
//   React Native (Hermes):       "    at render (address at index.android.bundle:1:4821)"
//   Firefox, Safari:             "render@https://shop.com/app.js:1:4821"
//
// The server's fingerprinting needs structured frames, so we parse both.
// Lines that match neither (like the "TypeError: ..." header) are skipped.

export type InAppFn = (filename: string) => boolean;

/** Default: everything is the app's own code except dependencies and engine internals. */
export const defaultIsInApp: InAppFn = (filename) =>
  !filename.includes("/node_modules/") && !filename.startsWith("<") && filename !== "native";

export function parseStack(stack: string | undefined, isInApp: InAppFn = defaultIsInApp): Frame[] {
  if (!stack) return [];
  const frames: Frame[] = [];
  for (const line of stack.split("\n")) {
    const f = parseV8Line(line) ?? parseGeckoLine(line);
    if (f) {
      frames.push({ ...f, in_app: f.filename ? isInApp(f.filename) : false });
    }
  }
  return frames;
}

type RawFrame = Omit<Frame, "in_app">;

function parseV8Line(line: string): RawFrame | null {
  const trimmed = line.trim();
  if (!trimmed.startsWith("at ")) return null;
  let rest = trimmed.slice(3);
  let fn = "";
  // "fn (location)": the location is inside the last parentheses.
  const open = rest.lastIndexOf(" (");
  if (rest.endsWith(")") && open >= 0) {
    fn = rest.slice(0, open);
    rest = rest.slice(open + 2, -1);
  }
  fn = fn.replace(/^async /, "");
  return { ...withFunction(fn), ...parseLocation(rest) };
}

function parseGeckoLine(line: string): RawFrame | null {
  const trimmed = line.trim();
  const at = trimmed.indexOf("@");
  if (at < 0) return null;
  const location = trimmed.slice(at + 1);
  // Require a line number, so "user@example.com" in a message isn't a frame.
  if (!/:\d+(:\d+)?$/.test(location)) return null;
  return { ...withFunction(trimmed.slice(0, at)), ...parseLocation(location) };
}

function withFunction(fn: string): Pick<Frame, "function"> {
  return fn ? { function: fn } : {};
}

function parseLocation(loc: string): Pick<Frame, "filename" | "lineno" | "colno"> {
  // Hermes (React Native's engine) writes release frames as
  // "address at index.android.bundle:1:2345".
  loc = loc.replace(/^address at /, "");
  const m = /^(.*?):(\d+)(?::(\d+))?$/.exec(loc);
  if (!m) return { filename: loc };
  const out: Pick<Frame, "filename" | "lineno" | "colno"> = { filename: m[1]!, lineno: Number(m[2]) };
  if (m[3]) out.colno = Number(m[3]);
  return out;
}

/**
 * Turns anything that was thrown into exception data. JavaScript lets you
 * throw any value (`throw "oops"`, `throw {code: 1}`), so we can't assume an
 * Error. We also duck-type instead of using `instanceof Error`, because an
 * Error from an iframe is an instance of that frame's Error, not ours.
 */
export function exceptionFromThrown(thrown: unknown, isInApp: InAppFn = defaultIsInApp): ExceptionData {
  if (isErrorLike(thrown)) {
    const frames = parseStack(thrown.stack, isInApp);
    const ex: ExceptionData = { type: thrown.name || "Error", message: String(thrown.message ?? "") };
    if (frames.length > 0) ex.stacktrace = { frames };
    return ex;
  }
  if (typeof thrown === "string") return { type: "Error", message: thrown };
  return { type: "Error", message: `Non-Error value thrown: ${describe(thrown)}` };
}

interface ErrorLike {
  name?: string;
  message?: unknown;
  stack?: string;
}

function isErrorLike(v: unknown): v is ErrorLike {
  return typeof v === "object" && v !== null && "message" in v && ("stack" in v || "name" in v);
}

function describe(v: unknown): string {
  try {
    const s = JSON.stringify(v);
    return s === undefined ? String(v) : s.slice(0, 500);
  } catch {
    return String(v); // circular objects can't be stringified
  }
}
