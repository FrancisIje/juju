import { useState } from "react";

// Copies text, and says so. If the browser refuses clipboard access, it
// selects the text instead so ⌘C still works.
export function CopyButton({ text, label = "Copy", className = "btn small" }: { text: string; label?: string; className?: string }) {
  const [done, setDone] = useState(false);
  async function copy(e: React.MouseEvent<HTMLButtonElement>) {
    try {
      await navigator.clipboard.writeText(text);
      setDone(true);
      setTimeout(() => setDone(false), 1500);
    } catch {
      const pre = e.currentTarget.closest(".code-wrap")?.querySelector("pre");
      if (pre) window.getSelection()?.selectAllChildren(pre);
    }
  }
  return (
    <button type="button" className={`${className} copy`} onClick={copy} aria-live="polite">
      {done ? "Copied" : label}
    </button>
  );
}

export function Code({ children }: { children: string }) {
  return (
    <div className="code-wrap">
      <pre className="code">{children}</pre>
      <CopyButton text={children} />
    </div>
  );
}
