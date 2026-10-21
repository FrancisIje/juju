const groups: [string, [string, string][]][] = [
  ["Anywhere", [["⌘ K", "Command palette: search issues, jump to pages"], ["?", "Show these shortcuts"], ["g  i", "Go to issues"], ["g  r", "Go to releases"], ["g  p", "Go to performance"], ["g  a", "Go to analytics"], ["g  s", "Go to settings"]]],
  ["Issue list", [["j / k", "Move down / up"], ["Enter or o", "Open the issue"], ["x", "Select for bulk actions"], ["r", "Resolve"], ["i", "Ignore"], ["u", "Mark unresolved"], ["/", "Search"]]],
  ["Performance", [["j / k", "Move down / up"], ["Enter or o", "Open the transaction"], ["[ / ]", "Previous / next sample"], ["← → on a chart", "Read each point"], ["Esc", "Back to the list"]]],
  ["Analytics", [["1 / 2 / 3", "Trends / funnels / retention"], ["← / →", "Move along the chart (when it has focus)"]]],
  ["Issue page", [["r / i / u", "Resolve / ignore / mark unresolved"], ["[ / ]", "Newer / older event"], ["1 – 4", "Stack trace, breadcrumbs, context, raw JSON"], ["Esc", "Back to the list"]]],
];

export function ShortcutsHelp({ onClose }: { onClose: () => void }) {
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()} onKeyDown={(e) => {
        // Stop here, so Esc closes this dialog without also leaving the page.
        if (e.key === "Escape") {
          e.stopPropagation();
          onClose();
        }
      }}>
      <div className="dialog" role="dialog" aria-label="Keyboard shortcuts" aria-modal="true" style={{ padding: "var(--space-5)", display: "grid", gap: "var(--space-4)" }}>
        <div className="page-head">
          <h2>Keyboard shortcuts</h2>
          <button className="btn ghost small" onClick={onClose} autoFocus>
            Close
          </button>
        </div>
        {groups.map(([title, keys]) => (
          <section key={title} style={{ display: "grid", gap: 6 }}>
            <h3 className="muted">{title}</h3>
            {keys.map(([k, what]) => (
              <div key={k} style={{ display: "flex", justifyContent: "space-between", gap: 12 }}>
                <span>{what}</span>
                <span>
                  {k.split("  ").map((part) => (
                    <kbd key={part} style={{ marginLeft: 4 }}>
                      {part}
                    </kbd>
                  ))}
                </span>
              </div>
            ))}
          </section>
        ))}
      </div>
    </div>
  );
}
