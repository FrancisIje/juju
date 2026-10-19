// The small slice of React Native's API this SDK uses. Declaring it here
// instead of installing react-native (hundreds of MB) as a dev dependency
// keeps the SDK workspace light. The app provides the real module at run time.
declare module "react-native" {
  export const Platform: {
    OS: "ios" | "android" | string;
    Version: string | number;
    isPad?: boolean;
    constants?: Record<string, unknown>;
  };
  export const Dimensions: {
    get(dim: "screen" | "window"): { width: number; height: number; scale: number; fontScale: number };
  };
  export type AppStateStatus = "active" | "background" | "inactive" | "unknown" | "extension";
  export const AppState: {
    currentState: AppStateStatus;
    addEventListener(type: "change", listener: (state: AppStateStatus) => void): { remove(): void };
  };
}
