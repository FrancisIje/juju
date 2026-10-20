import { createContext, useContext } from "react";
import type { Me } from "./api";

export interface AppState {
  me: Me;
  reloadMe: () => Promise<void>;
  toast: (message: string) => void;
}

export const AppContext = createContext<AppState | null>(null);

export function useApp(): AppState {
  const v = useContext(AppContext);
  if (!v) throw new Error("useApp outside AppContext");
  return v;
}
