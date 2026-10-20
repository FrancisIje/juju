import { useEffect, useMemo, useRef, useState } from "react";
import { api, type IssueRow } from "../lib/api";
import { navigate } from "../lib/router";

export interface Command {
  id: string;
  label: string;
  group: string;
  hint?: string;
  run: () => void;
}

/** Filters commands by every word typed, so "set al" finds "Settings: Alerts". */
export function filterCommands(commands: Command[], query: string): Command[] {
  const words = query.toLowerCase().split(/\s+/).filter(Boolean);
  if (words.length === 0) return commands;
  return commands.filter((c) => {
    const text = `${c.group} ${c.label}`.toLowerCase();
    return words.every((w) => text.includes(w));
  });
}

// ⌘K: jump anywhere by typing. Issues matching the text are searched live
// in the current project; everything else is a fixed list of commands.
export function CommandPalette({ commands, projectId, onClose }: { commands: Command[]; projectId: number | null; onClose: () => void }) {
  const [query, setQuery] = useState("");
  const [active, setActive] = useState(0);
  const [issues, setIssues] = useState<IssueRow[]>([]);
  const input = useRef<HTMLInputElement>(null);

  useEffect(() => input.current?.focus(), []);

  useEffect(() => {
    if (!projectId || query.trim().length < 2) {
      setIssues([]);
      return;
    }
    const ctrl = new AbortController();
    const t = setTimeout(() => {
      api<{ issues: IssueRow[] }>(`/api/ui/projects/${projectId}/issues?status=all&limit=5&q=${encodeURIComponent(query)}`, { signal: ctrl.signal })
        .then((r) => setIssues(r.issues))
        .catch(() => {});
    }, 150);
    return () => {
      clearTimeout(t);
      ctrl.abort();
    };
  }, [query, projectId]);

  const items = useMemo(() => {
    const issueCommands: Command[] = issues.map((i) => ({
      id: `issue-${i.id}`,
      group: "Issues",
      label: i.title,
      hint: `#${i.id}`,
      run: () => navigate(`/p/${projectId}/issues/${i.id}`),
    }));
    return [...issueCommands, ...filterCommands(commands, query)];
  }, [issues, commands, query, projectId]);

  useEffect(() => setActive(0), [query]);

  function onKey(e: React.KeyboardEvent) {
    if (e.key === "ArrowDown") {
      e.preventDefault();
      setActive((a) => Math.min(items.length - 1, a + 1));
    } else if (e.key === "ArrowUp") {
      e.preventDefault();
      setActive((a) => Math.max(0, a - 1));
    } else if (e.key === "Enter") {
      e.preventDefault();
      const item = items[active];
      if (item) {
        onClose();
        item.run();
      }
    } else if (e.key === "Escape") {
      e.stopPropagation();
      onClose();
    }
  }

  let lastGroup = "";
  return (
    <div className="overlay" onMouseDown={(e) => e.target === e.currentTarget && onClose()}>
      <div className="dialog" role="dialog" aria-label="Command palette" aria-modal="true">
        <input
          ref={input}
          className="palette-input"
          placeholder="Search issues, or jump to a page…"
          value={query}
          onChange={(e) => setQuery(e.target.value)}
          onKeyDown={onKey}
          aria-label="Search commands"
          aria-controls="palette-list"
          aria-activedescendant={items[active] ? `cmd-${items[active]!.id}` : undefined}
          id="palette-input"
        />
        <ul className="palette-list" id="palette-list" role="listbox">
          {items.length === 0 && <li className="group">Nothing matches “{query}”.</li>}
          {items.map((c, i) => {
            const header = c.group !== lastGroup ? <li className="group" key={`g-${c.group}`} role="presentation">{c.group}</li> : null;
            lastGroup = c.group;
            return [
              header,
              <li
                key={c.id}
                id={`cmd-${c.id}`}
                role="option"
                aria-selected={i === active}
                onMouseEnter={() => setActive(i)}
                onClick={() => {
                  onClose();
                  c.run();
                }}
              >
                <span>{c.label}</span>
                {c.hint && <span className="faint mono">{c.hint}</span>}
              </li>,
            ];
          })}
        </ul>
      </div>
    </div>
  );
}
