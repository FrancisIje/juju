import { afterEach, describe, expect, it, vi } from "vitest";
import { Client } from "../src/client";
import { debugImages } from "../src/debugids";
import type { SendResult } from "../src/transport";
import type { Envelope, JujuEvent } from "../src/types";
// The snippet builds put in each bundle. The Go side (server/internal/debugid)
// checks its copy against this same file. (?raw: Vite loads it as text.)
import template from "../../../server/internal/debugid/testdata/snippet.txt?raw";

const g = globalThis as { __jujuDebugIds?: Record<string, string> };
afterEach(() => {
  delete g.__jujuDebugIds;
});

describe("debug ids", () => {
  it("the real snippet records its id under its own stack, and never throws", () => {
    new Function(template.replace("{{ID}}", "4f1a2b3c-0000-4d5e-8f90-abcdef012345"))();
    const entries = Object.entries(g.__jujuDebugIds ?? {});
    expect(entries).toHaveLength(1);
    expect(entries[0]![1]).toBe("4f1a2b3c-0000-4d5e-8f90-abcdef012345");
    expect(entries[0]![0]).toMatch(/^Error/); // a stack trace
    // Where Error has no stack at all, it records nothing and doesn't throw.
    const RealError = globalThis.Error;
    try {
      (globalThis as { Error: unknown }).Error = function () {
        return {};
      };
      expect(() => new Function(template.replace("{{ID}}", "x"))()).not.toThrow();
    } finally {
      globalThis.Error = RealError;
    }
  });

  it("finds each frame's bundle in stacks from Chrome, Safari and Hermes", () => {
    g.__jujuDebugIds = {
      "Error\n    at https://shop.com/assets/app.min.js:1:61\n    at https://shop.com/assets/app.min.js:1:120": "id-app",
      "@https://cdn.shop.com/vendor.js:1:61\nglobal code@https://cdn.shop.com/vendor.js:1:131": "id-vendor", // Safari
      "Error\n    at anonymous (address at index.android.bundle:1:834)\n    at global (address at index.android.bundle:1:901)": "id-rn", // Hermes
    };
    const images = debugImages([
      { function: "a", filename: "https://shop.com/assets/app.min.js", lineno: 2, colno: 10, in_app: true },
      { function: "b", filename: "https://cdn.shop.com/vendor.js", lineno: 2, colno: 3, in_app: true },
      { function: "c", filename: "https://shop.com/assets/app.min.js", lineno: 2, colno: 40, in_app: true }, // same file again
      { function: "d", filename: "index.android.bundle", lineno: 1, colno: 9, in_app: true },
      { function: "Array.map", filename: "<anonymous>", in_app: false },
    ]);
    expect(images).toEqual([
      { type: "sourcemap", code_file: "https://shop.com/assets/app.min.js", debug_id: "id-app" },
      { type: "sourcemap", code_file: "https://cdn.shop.com/vendor.js", debug_id: "id-vendor" },
      { type: "sourcemap", code_file: "index.android.bundle", debug_id: "id-rn" },
    ]);
  });

  it("nothing stamped: no images", () => {
    expect(debugImages([{ filename: "https://shop.com/app.js", lineno: 1, colno: 1, in_app: true }])).toEqual([]);
  });

  it("errors carry debug_meta for the files in their stack only", async () => {
    g.__jujuDebugIds = {
      "Error\n    at https://shop.com/app.min.js:1:61": "4f1a2b3c-0000-4d5e-8f90-abcdef012345",
      "Error\n    at https://shop.com/unrelated.js:1:61": "00000000-0000-4000-8000-000000000009",
    };
    const sent: Envelope[] = [];
    const c = new Client({ dsn: "https://pk_abc@juju.example.com/1", transport: { send: vi.fn(async (e: Envelope): Promise<SendResult> => (sent.push(e), { ok: true })) } });
    const err = new Error("boom");
    err.stack = "Error: boom\n    at checkout (https://shop.com/app.min.js:2:4821)\n    at https://shop.com/app.min.js:2:90";
    c.captureException(err);
    await c.flush();
    const ev = sent.flatMap((e) => e.events).find((e) => e.type === "error") as JujuEvent;
    expect(ev.debug_meta).toEqual({ images: [{ type: "sourcemap", code_file: "https://shop.com/app.min.js", debug_id: "4f1a2b3c-0000-4d5e-8f90-abcdef012345" }] });
  });
});
