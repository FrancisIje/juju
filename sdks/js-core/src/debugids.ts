import { parseStack } from "./stacktrace";
import type { DebugImage, Frame } from "./types";

// Debug ids tie each bundle to its own source map (server/internal/debugid
// explains why). The build adds a one-line snippet to each bundle, either
// `juju sourcemaps inject` (web) or the Metro plugin (React Native), that
// runs first and records its id under its own stack trace:
//
//   globalThis.__jujuDebugIds["Error\n    at https://shop.com/app.min.js:1:40"] = "4f1a…"
//
// The stack says which file the snippet ran in, so even with several
// bundles on a page, each id is matched to the right file.

const fileOf = new Map<string, string | undefined>(); // stack → file, parsed once each

function registry(): Record<string, unknown> | undefined {
  try {
    const ids = (globalThis as { __jujuDebugIds?: unknown }).__jujuDebugIds;
    return ids && typeof ids === "object" ? (ids as Record<string, unknown>) : undefined;
  } catch {
    return undefined;
  }
}

/** The debug images for the files these frames are in. Empty if no bundle was stamped. */
export function debugImages(frames: Frame[]): DebugImage[] {
  const ids = registry();
  if (!ids) return [];
  const byFile = new Map<string, string>();
  for (const [stack, id] of Object.entries(ids)) {
    if (typeof id !== "string") continue;
    if (!fileOf.has(stack)) fileOf.set(stack, parseStack(stack).find((f) => f.filename)?.filename);
    const file = fileOf.get(stack);
    if (file) byFile.set(file, id);
  }
  const images: DebugImage[] = [];
  const seen = new Set<string>();
  for (const f of frames) {
    const id = f.filename ? byFile.get(f.filename) : undefined;
    if (f.filename && id && !seen.has(f.filename)) {
      seen.add(f.filename);
      images.push({ type: "sourcemap", code_file: f.filename, debug_id: id });
    }
  }
  return images;
}
