// A stand-in for the react-native module, which can't run in Node. Tests
// change these values to simulate iOS or Android.
type Listener = (state: string) => void;
const listeners = new Set<Listener>();

export const Platform = {
  OS: "ios",
  Version: "18.2",
  isPad: false,
  constants: { interfaceIdiom: "phone", reactNativeVersion: { major: 0, minor: 87, patch: 1 } } as Record<string, unknown>,
};

export const Dimensions = {
  get: () => ({ width: 402.4, height: 874, scale: 3, fontScale: 1 }),
};

export const AppState = {
  currentState: "active",
  addEventListener(_type: "change", listener: Listener) {
    listeners.add(listener);
    return { remove: () => listeners.delete(listener) };
  },
};

/** Test helper: pretend the OS changed the app's state. */
export function emitAppState(state: string): void {
  for (const l of [...listeners]) l(state);
}
