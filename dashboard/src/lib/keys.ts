// Keyboard shortcuts. They never fire while someone is typing in a field,
// so "r" in a search box is just the letter r.
import { useEffect, useRef } from "react";

export function isTyping(target: EventTarget | null): boolean {
  const el = target as HTMLElement | null;
  if (!el) return false;
  return el.isContentEditable || ["INPUT", "TEXTAREA", "SELECT"].includes(el.tagName);
}

/** Map of key → handler. Keys are KeyboardEvent.key values, plus "mod+k" for ⌘K / Ctrl+K. */
export function useShortcuts(map: Record<string, (e: KeyboardEvent) => void>, enabled = true) {
  const ref = useRef(map);
  ref.current = map;
  useEffect(() => {
    if (!enabled) return;
    function onKey(e: KeyboardEvent) {
      if ((e.metaKey || e.ctrlKey) && e.key.toLowerCase() === "k") {
        const h = ref.current["mod+k"];
        if (h) {
          e.preventDefault();
          h(e);
        }
        return;
      }
      if (e.metaKey || e.ctrlKey || e.altKey || isTyping(e.target)) return;
      const h = ref.current[e.key];
      if (h) {
        e.preventDefault();
        h(e);
      }
    }
    window.addEventListener("keydown", onKey);
    return () => window.removeEventListener("keydown", onKey);
  }, [enabled]);
}
