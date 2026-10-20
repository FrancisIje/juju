package symbolicate

import (
	"fmt"
	"regexp"
	"strings"
	"sync"

	"github.com/go-sourcemap/sourcemap"
)

// A source map records, for each position in a minified file, the
// position in the original source it came from, plus the original
// identifier there. It maps positions only: it doesn't say which function
// a position is inside. sourceMap adds that.
type sourceMap struct {
	c *sourcemap.Consumer
	// Original source split into lines, from the map's sourcesContent.
	// Filled on first use; the mutex makes that safe when several workers
	// share one parsed map.
	mu    sync.Mutex
	lines map[string][]string
}

func parseSourceMap(name string, data []byte) (*sourceMap, error) {
	c, err := sourcemap.Parse(name, data)
	if err != nil {
		return nil, fmt.Errorf("not a valid source map: %w", err)
	}
	return &sourceMap{c: c, lines: map[string][]string{}}, nil
}

// lookup takes the line and column a JavaScript engine reports (both
// 1-based) and returns the original position. The source map library
// counts columns from 0, hence the -1 and +1.
func (m *sourceMap) lookup(line, col int) (loc location, name string, ok bool) {
	if line < 1 || col < 1 {
		return location{}, "", false
	}
	file, name, l, c, ok := m.c.Source(line, col-1)
	if !ok || file == "" {
		return location{}, "", false
	}
	return location{File: file, Line: l, Col: c + 1}, name, true
}

var (
	// function lineTotal(  ·  async function* gen(
	fnDecl = regexp.MustCompile(`\bfunction\s*\*?\s*([A-Za-z_$][\w$]*)\s*\(`)
	// const lineTotal = (item) =>  ·  let f = async function (  ·  var g = x =>
	fnAssign = regexp.MustCompile(`\b(?:const|let|var)\s+([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?(?:function\b|\([^)]*\)\s*=>|[A-Za-z_$][\w$]*\s*=>)`)
	// lineTotal(item) {  ·  async load() {  ·  static from(x) {   (class and object methods)
	fnMethod = regexp.MustCompile(`^\s*(?:async\s+|static\s+|get\s+|set\s+)*([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{`)
	// onClick = (event) => {   (class fields)
	fnField = regexp.MustCompile(`^\s*([A-Za-z_$][\w$]*)\s*=\s*(?:async\s+)?\([^)]*\)\s*=>`)
)

var notFunctions = map[string]bool{"if": true, "for": true, "while": true, "switch": true, "catch": true, "with": true, "return": true, "function": true}

// enclosingFunction guesses the original function around a position by
// reading the original source upward, counting braces, until it finds the
// unclosed "{" of a function header. It's a heuristic: braces inside
// strings or comments can fool it. It's used only when the caller's
// call site (the exact method) isn't available.
func (m *sourceMap) enclosingFunction(file string, line, col int) string {
	lines := m.sourceLines(file)
	if line < 1 || line > len(lines) {
		return ""
	}
	// An arrow function with no braces is all on one line.
	if n := headerName(lines[line-1]); n != "" && !strings.Contains(lines[line-1], "{") {
		return n
	}
	depth := 0
	for i := line - 1; i >= 0; i-- {
		text := lines[i]
		if i == line-1 && col-1 < len(text) && col > 0 {
			text = text[:col-1] // only what comes before the error on its own line
		}
		for j := len(text) - 1; j >= 0; j-- {
			switch text[j] {
			case '}':
				depth++
			case '{':
				if depth > 0 {
					depth--
					continue
				}
				// An unclosed "{": the start of a block we're inside. If its
				// line (or the line before, for "{" on its own line) is a
				// function header, that's our function; otherwise (if, for…)
				// keep going outward.
				header := text[:j+1]
				if n := headerName(header); n != "" {
					return n
				}
				if strings.TrimSpace(text[:j]) == "" && i > 0 {
					if n := headerName(lines[i-1] + "{"); n != "" {
						return n
					}
				}
			}
		}
	}
	return ""
}

func headerName(text string) string {
	for _, re := range []*regexp.Regexp{fnDecl, fnAssign, fnField, fnMethod} {
		if m := re.FindAllStringSubmatch(text, -1); m != nil {
			n := m[len(m)-1][1] // the last header on the line is the innermost
			if !notFunctions[n] {
				return n
			}
		}
	}
	return ""
}

func (m *sourceMap) sourceLines(file string) []string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if l, ok := m.lines[file]; ok {
		return l
	}
	if m.c == nil {
		return nil
	}
	l := strings.Split(m.c.SourceContent(file), "\n")
	m.lines[file] = l
	return l
}
