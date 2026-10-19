import { Dimensions, Platform } from "react-native";

/**
 * Device, OS and runtime details attached to every event, from React
 * Native's built-in Platform and Dimensions modules. No native module is
 * needed, so the SDK installs with npm alone (no pods, no rebuild).
 *
 * Privacy: Android's Platform.constants also has a hardware serial number
 * and build fingerprint. Those identify a physical device, so we leave
 * them out.
 */
export function deviceContext(): Record<string, unknown> {
  const c = (Platform.constants ?? {}) as Record<string, unknown>;
  const screen = Dimensions.get("screen");
  const rn = c.reactNativeVersion as { major?: number; minor?: number; patch?: number } | undefined;
  const g = globalThis as { HermesInternal?: unknown; __DEV__?: boolean };
  return clean({
    // On Android, Platform.Version is the API level (37), not the version
    // people know (17), which is constants.Release. Report both.
    os:
      Platform.OS === "android"
        ? { name: "android", version: String(c.Release ?? Platform.Version), api_level: Number(Platform.Version) }
        : { name: Platform.OS, version: String(Platform.Version) },
    device: {
      model: c.Model, // Android
      manufacturer: c.Manufacturer, // Android
      brand: c.Brand, // Android
      idiom: c.interfaceIdiom, // iOS: "phone", "pad"
      is_tablet: Platform.isPad,
    },
    screen: { width: Math.round(screen.width), height: Math.round(screen.height), scale: screen.scale },
    runtime: {
      name: "react-native",
      version: rn ? `${rn.major}.${rn.minor}.${rn.patch}` : undefined,
      engine: g.HermesInternal ? "hermes" : "jsc",
      dev: Boolean(g.__DEV__),
    },
  });
}

/** Drops undefined values, so iOS events don't carry empty Android fields. */
function clean<T extends Record<string, unknown>>(obj: T): T {
  const out: Record<string, unknown> = {};
  for (const [k, v] of Object.entries(obj)) {
    if (v === undefined) continue;
    out[k] = v !== null && typeof v === "object" && !Array.isArray(v) ? clean(v as Record<string, unknown>) : v;
  }
  return out as T;
}

/**
 * Which stack frames are the app's own code. In a release build all
 * JavaScript, React Native's included, is in one bundle file, so the
 * filename can't tell them apart yet (source maps in Phase 5 will). What we
 * can exclude is the engine itself: Hermes's built-in code
 * (InternalBytecode.js) and native functions.
 */
export function isInApp(filename: string): boolean {
  if (filename === "native" || filename.startsWith("[native")) return false;
  if (filename.endsWith("InternalBytecode.js")) return false;
  return !filename.includes("/node_modules/");
}
