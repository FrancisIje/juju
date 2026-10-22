import { mkdtempSync, readdirSync, readFileSync, utimesSync, writeFileSync } from "node:fs";
import { createRequire } from "node:module";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { describe, expect, it } from "vitest";

const require = createRequire(import.meta.url);
const { withJujuConfig, snippet } = require("../metro.cjs");
const template = readFileSync(new URL("../../../server/internal/debugid/testdata/snippet.txt", import.meta.url), "utf8");

describe("Metro plugin", () => {
  it("uses exactly the snippet the server recognizes", () => {
    expect(snippet("{{ID}}")).toBe(template);
  });

  it("adds a polyfill with a new debug id, after the app's own polyfills", () => {
    const root = mkdtempSync(join(tmpdir(), "juju-metro-"));
    const cfg = withJujuConfig({ projectRoot: root, serializer: { getPolyfills: () => ["/app/polyfill.js"], other: 1 } });
    const polyfills = cfg.serializer.getPolyfills({ platform: "android" });
    expect(polyfills[0]).toBe("/app/polyfill.js");
    expect(polyfills).toHaveLength(2);
    expect(cfg.serializer.other).toBe(1); // the rest of the config is kept
    // Inside the project, where Metro can read it.
    expect(polyfills[1].startsWith(join(root, "node_modules", ".cache", "juju"))).toBe(true);
    const src = readFileSync(polyfills[1], "utf8");
    const id = /\[s\]="([0-9a-f-]{36})"/.exec(src)?.[1];
    expect(id).toMatch(/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
    expect(src).toContain(snippet(id));

    // Each Metro process, so each build, gets its own id.
    const again = withJujuConfig({ projectRoot: root }).serializer.getPolyfills({});
    expect(readFileSync(again[0], "utf8")).not.toContain(id);
  });

  it("works on a config without a serializer section", () => {
    const root = mkdtempSync(join(tmpdir(), "juju-metro-"));
    expect(withJujuConfig({ projectRoot: root }).serializer.getPolyfills({})).toHaveLength(1);
  });

  it("removes files from builds more than a day old", () => {
    const root = mkdtempSync(join(tmpdir(), "juju-metro-"));
    const dir = join(root, "node_modules", ".cache", "juju");
    withJujuConfig({ projectRoot: root });
    const old = join(dir, "debug-id-1-1.js");
    writeFileSync(old, "x");
    const twoDaysAgo = (Date.now() - 2 * 86_400_000) / 1000;
    utimesSync(old, twoDaysAgo, twoDaysAgo);
    withJujuConfig({ projectRoot: root });
    expect(readdirSync(dir)).not.toContain("debug-id-1-1.js");
    expect(readdirSync(dir).length).toBe(2); // this run's and the previous one's
  });
});
