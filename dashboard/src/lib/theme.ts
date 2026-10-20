// Theme: follows the OS by default; the toggle stores an override. Storage
// can be blocked (private windows), so every access is guarded and the app
// works without it.
export type Theme = "system" | "dark" | "light";
const KEY = "juju-theme";

export function loadTheme(): Theme {
  try {
    const t = localStorage.getItem(KEY);
    return t === "dark" || t === "light" ? t : "system";
  } catch {
    return "system";
  }
}

export function applyTheme(t: Theme) {
  if (t === "system") document.documentElement.removeAttribute("data-theme");
  else document.documentElement.setAttribute("data-theme", t);
  try {
    if (t === "system") localStorage.removeItem(KEY);
    else localStorage.setItem(KEY, t);
  } catch {
    // not remembered, still applied
  }
}

export function nextTheme(t: Theme): Theme {
  return t === "system" ? "dark" : t === "dark" ? "light" : "system";
}
