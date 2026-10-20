import { filterCommands, type Command } from "../src/components/CommandPalette";
import { flatten } from "../src/components/issue/Context";
import type { Frame } from "../src/lib/api";
import { groupFrames, location, splitTitle } from "../src/lib/frames";
import { match, safeNext } from "../src/lib/router";
import { snippet } from "../src/lib/snippets";
import { bytes, compact, relative } from "../src/lib/time";

describe("relative time", () => {
  const now = Date.parse("2026-10-20T12:00:00Z");
  test.each([
    ["2026-10-20T11:59:30Z", "just now"],
    ["2026-10-20T11:57:00Z", "3m ago"],
    ["2026-10-20T10:00:00Z", "2h ago"],
    ["2026-10-15T12:00:00Z", "5d ago"],
    ["2026-07-20T12:00:00Z", "3mo ago"],
    ["2024-10-20T12:00:00Z", "2y ago"],
    ["2026-10-20T12:05:00Z", "just now"], // a clock slightly ahead never shows "in the future"
    ["not a date", ""],
  ])("%s → %s", (iso, want) => expect(relative(iso, now)).toBe(want));
});

test.each([
  [999, "999"],
  [1000, "1k"],
  [1234, "1.2k"],
  [15_400, "15k"],
  [2_500_000, "2.5M"],
])("compact(%d) = %s", (n, want) => expect(compact(n)).toBe(want));

test("bytes", () => {
  expect(bytes(512)).toBe("512 B");
  expect(bytes(2048)).toBe("2.0 KB");
  expect(bytes(3 * 1024 * 1024)).toBe("3.0 MB");
});

describe("groupFrames", () => {
  const app = (n: string): Frame => ({ function: n, in_app: true });
  const lib = (n: string): Frame => ({ function: n, in_app: false });

  test("folds runs of two or more library frames, keeps single ones", () => {
    const g = groupFrames([app("a"), lib("l1"), lib("l2"), lib("l3"), app("b"), lib("l4")], false);
    expect(g.map((x) => (x.kind === "frame" ? x.frame.function : `${x.frames.length} hidden from ${x.start}`))).toEqual(["a", "3 hidden from 1", "b", "l4"]);
  });

  test("shows everything when asked, or when no frame is in-app", () => {
    expect(groupFrames([app("a"), lib("l1"), lib("l2")], true)).toHaveLength(3);
    // Hiding every frame would hide the only clues.
    expect(groupFrames([lib("l1"), lib("l2")], false).every((x) => x.kind === "frame")).toBe(true);
  });
});

test("location and splitTitle", () => {
  expect(location({ filename: "app.js", lineno: 4, colno: 9 })).toBe("app.js:4:9");
  expect(location({ filename: "app.js", colno: 9 })).toBe("app.js");
  expect(location({})).toBe("");
  expect(splitTitle("TypeError: x is undefined")).toEqual(["TypeError", "x is undefined"]);
  expect(splitTitle("just a message")).toEqual(["just a message", ""]);
});

test("command filter matches every word, in any order", () => {
  const cmds: Command[] = [
    { id: "a", group: "Go to", label: "Settings: Alerts", run: () => {} },
    { id: "k", group: "Go to", label: "Settings: Keys", run: () => {} },
  ];
  expect(filterCommands(cmds, "set al").map((c) => c.id)).toEqual(["a"]);
  expect(filterCommands(cmds, "alerts go").map((c) => c.id)).toEqual(["a"]);
  expect(filterCommands(cmds, "  ")).toHaveLength(2);
  expect(filterCommands(cmds, "billing")).toHaveLength(0);
});

test("router match", () => {
  expect(match("/p/:project/issues/:issue", "/p/3/issues/42")).toEqual({ project: "3", issue: "42" });
  expect(match("/p/:project/issues", "/p/3/issues/42")).toBeNull();
  expect(match("/p/:project/issues", "/x/3/issues")).toBeNull();
});

test("flatten nested context", () => {
  expect(flatten({ device: { model: "Pixel", ram: 8 }, ok: true, list: [1] })).toEqual([
    ["device.model", "Pixel"],
    ["device.ram", "8"],
    ["ok", "true"],
    ["list", "[1]"],
  ]);
});

test("curl snippet is built from the DSN", () => {
  const { code } = snippet("curl", "http://pk_abc@localhost:8080/7");
  expect(code).toContain('"http://localhost:8080/api/v1/projects/7/events"');
  expect(code).toContain("X-Juju-Key: pk_abc");
});

test("after login, ?next= only ever leads to a page on this site", () => {
  const o = "https://juju.example.com";
  expect(safeNext("/p/3/issues?status=all", o)).toBe("/p/3/issues?status=all");
  expect(safeNext(null, o)).toBe("/");
  // Each of these is read by browsers as another site.
  for (const bad of ["//evil.example", "/\\evil.example", "https://evil.example/p/1", "\\\\evil.example", "javascript:alert(1)"]) {
    expect(safeNext(bad, o), bad).toBe("/");
  }
});
