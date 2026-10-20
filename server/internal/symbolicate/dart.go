package symbolicate

import (
	"bytes"
	"debug/dwarf"
	"debug/elf"
	"debug/macho"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// Flutter apps built with --split-debug-info (and usually --obfuscate) print
// stack traces as addresses only:
//
//	build_id: 'b4485c2e…'
//	#00 abs 00000001052b4c67 virt 0000000000044c67 _kDartIsolateSnapshotInstructions+0x34c67
//
// The debug file saved at build time holds DWARF: the standard debug format
// compilers use to map machine-code addresses back to functions, files and
// lines. It's an ELF file for Android and a Mach-O file for iOS and macOS,
// and Go's standard library reads both.

// dartSymbols is one parsed debug file, indexed for address lookups.
type dartSymbols struct {
	buildID string
	// Where each snapshot's instructions start: frames are written as
	// "_kDartIsolateSnapshotInstructions+0x34c67", so address = start + offset.
	instructions map[string]uint64
	data         *dwarf.Data
	funcs        []dartFunc // sorted by low address
	// Every line-table row of every compile unit, sorted by address. Built
	// once at load time; after that the whole struct is read-only, so
	// workers can look up addresses concurrently without locks.
	rows []lineRow
	// Each compile unit's file table, which inlined call sites index into.
	files map[dwarf.Offset][]string
}

// lineRow says: from addr up to the next row's addr, the code is file:line:col.
// An end row marks the end of a sequence, so the addresses after it (until
// the next row) belong to no line.
type lineRow struct {
	addr      uint64
	file      string
	line, col int
	end       bool
}

// dartFunc is one address range of a compiled function, with the functions
// the compiler inlined into it.
type dartFunc struct {
	low, high uint64
	name      string
	cu        *dwarf.Entry
	inlines   []dartInline
	marks     internalMarks
}

// internalMarks follow Dart's own decoder (package:native_stack_traces):
// the compiler marks generated helpers (closure call stubs, a function
// "inlined" into itself) as artificial, and those frames are hidden.
type internalMarks struct {
	artificial bool
	lowPC      uint64
	hasLowPC   bool
}

func marksOf(e *dwarf.Entry) internalMarks {
	m := internalMarks{}
	m.artificial, _ = e.Val(dwarf.AttrArtificial).(bool)
	m.lowPC, m.hasLowPC = e.Val(dwarf.AttrLowpc).(uint64)
	return m
}

// dartInline is a function the compiler copied into its caller. One address
// then stands for two (or more) functions, and Dart's own decoder reports
// each as a frame, so we do too.
type dartInline struct {
	ranges   [][2]uint64
	name     string
	callFile int64
	callLine int64
	callCol  int64
	depth    int
	marks    internalMarks
}

// location is a source position, from either kind of debug file.
type location struct {
	Function string
	File     string
	Line     int
	Col      int
}

func parseDartSymbols(data []byte) (*dartSymbols, error) {
	var (
		d       *dwarf.Data
		buildID string
		syms    = map[string]uint64{}
		err     error
	)
	switch {
	case bytes.HasPrefix(data, []byte("\x7fELF")): // Android
		f, ferr := elf.NewFile(bytes.NewReader(data))
		if ferr != nil {
			return nil, ferr
		}
		buildID = elfBuildID(f)
		all, _ := f.Symbols()
		dyn, _ := f.DynamicSymbols()
		for _, s := range append(all, dyn...) {
			if strings.HasPrefix(s.Name, "_kDart") && s.Value != 0 {
				syms[s.Name] = s.Value
			}
		}
		d, err = f.DWARF()
	case len(data) >= 4 && binary.LittleEndian.Uint32(data) == macho.Magic64: // iOS, macOS
		f, ferr := macho.NewFile(bytes.NewReader(data))
		if ferr != nil {
			return nil, ferr
		}
		buildID = machoUUID(f)
		if f.Symtab != nil {
			for _, s := range f.Symtab.Syms {
				if strings.HasPrefix(s.Name, "_kDart") && s.Value != 0 {
					syms[s.Name] = s.Value
				}
			}
		}
		d, err = f.DWARF()
	default:
		return nil, errors.New("not a Dart debug file: expected ELF (Android) or Mach-O (iOS, macOS)")
	}
	if err != nil {
		return nil, fmt.Errorf("read DWARF: %w", err)
	}
	if buildID == "" {
		return nil, errors.New("the debug file has no build id")
	}
	s := &dartSymbols{buildID: buildID, instructions: syms, data: d, files: map[dwarf.Offset][]string{}}
	if err := s.index(); err != nil {
		return nil, err
	}
	return s, nil
}

// index walks the DWARF tree once and records every function's address
// ranges and the functions inlined into it, so lookups are a binary search.
func (s *dartSymbols) index() error {
	// Concrete functions often have no name of their own: they point at an
	// "abstract" entry that does (DW_AT_abstract_origin). Collect names first.
	names := map[dwarf.Offset]string{}
	r := s.data.Reader()
	for {
		e, err := r.Next()
		if err != nil {
			return fmt.Errorf("read DWARF: %w", err)
		}
		if e == nil {
			break
		}
		if n, ok := e.Val(dwarf.AttrName).(string); ok {
			names[e.Offset] = n
		}
	}
	nameOf := func(e *dwarf.Entry) string {
		if n, ok := e.Val(dwarf.AttrName).(string); ok {
			return n
		}
		for _, a := range []dwarf.Attr{dwarf.AttrAbstractOrigin, dwarf.AttrSpecification} {
			if o, ok := e.Val(a).(dwarf.Offset); ok {
				return names[o]
			}
		}
		return ""
	}

	r = s.data.Reader()
	var cu *dwarf.Entry
	var current []int // indexes into s.funcs of the function being walked
	depth, funcDepth := 0, -1
	for {
		e, _ := r.Next()
		if e == nil {
			break
		}
		if e.Tag == 0 { // end of a list of children
			depth--
			if depth <= funcDepth {
				current, funcDepth = nil, -1
			}
			continue
		}
		switch e.Tag {
		case dwarf.TagCompileUnit:
			cu = e
			if err := s.indexLines(e); err != nil {
				return err
			}
		case dwarf.TagSubprogram:
			ranges, _ := s.data.Ranges(e)
			if len(ranges) > 0 {
				current = current[:0:0]
				for _, rg := range ranges {
					s.funcs = append(s.funcs, dartFunc{low: rg[0], high: rg[1], name: nameOf(e), cu: cu, marks: marksOf(e)})
					current = append(current, len(s.funcs)-1)
				}
				funcDepth = depth
			}
		case dwarf.TagInlinedSubroutine:
			if current != nil {
				ranges, _ := s.data.Ranges(e)
				in := dartInline{ranges: ranges, name: nameOf(e), depth: depth - funcDepth, marks: marksOf(e)}
				in.callFile, _ = e.Val(dwarf.AttrCallFile).(int64)
				in.callLine, _ = e.Val(dwarf.AttrCallLine).(int64)
				in.callCol, _ = e.Val(dwarf.AttrCallColumn).(int64)
				for _, i := range current {
					s.funcs[i].inlines = append(s.funcs[i].inlines, in)
				}
			}
		}
		if e.Children {
			depth++
		}
	}
	sort.Slice(s.funcs, func(i, j int) bool { return s.funcs[i].low < s.funcs[j].low })
	// Sort rows by address. Where a sequence ends exactly where the next one
	// starts, put the end row first so the start row wins.
	sort.SliceStable(s.rows, func(i, j int) bool {
		if s.rows[i].addr != s.rows[j].addr {
			return s.rows[i].addr < s.rows[j].addr
		}
		return s.rows[i].end && !s.rows[j].end
	})
	return nil
}

// indexLines reads a compile unit's whole line table. We don't keep a
// dwarf.LineReader around for lookups: its SeekPC scans forward from its
// current position, which fails when sequences aren't in address order
// (as in Flutter's Android symbol files), and a shared reader isn't safe
// for concurrent use.
func (s *dartSymbols) indexLines(cu *dwarf.Entry) error {
	lr, err := s.data.LineReader(cu)
	if err != nil || lr == nil {
		return err
	}
	var names []string
	for _, f := range lr.Files() {
		if f == nil {
			names = append(names, "")
		} else {
			names = append(names, f.Name)
		}
	}
	s.files[cu.Offset] = names
	var le dwarf.LineEntry
	for lr.Next(&le) == nil {
		row := lineRow{addr: le.Address, end: le.EndSequence}
		if !le.EndSequence {
			row.line, row.col = le.Line, le.Column
			if le.File != nil {
				row.file = le.File.Name
			}
		}
		s.rows = append(s.rows, row)
	}
	return nil
}

// lineAt finds the source line for an address with a binary search.
func (s *dartSymbols) lineAt(addr uint64) (lineRow, bool) {
	i := sort.Search(len(s.rows), func(i int) bool { return s.rows[i].addr > addr }) - 1
	if i < 0 || s.rows[i].end {
		return lineRow{}, false
	}
	return s.rows[i], true
}

// address turns "_kDartIsolateSnapshotInstructions+0x34c67" (or a bare
// "0x44c67" virtual address) into an address in the debug file.
func (s *dartSymbols) address(symbolic string) (uint64, bool) {
	if v, ok := strings.CutPrefix(symbolic, "0x"); ok {
		n, err := strconv.ParseUint(v, 16, 64)
		return n, err == nil
	}
	sym, off, ok := strings.Cut(symbolic, "+0x")
	if !ok {
		return 0, false
	}
	start, ok := s.instructions[sym]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseUint(off, 16, 64)
	return start + n, err == nil
}

// lookup returns the source locations for one address, innermost first.
// One address can give several frames when the compiler inlined functions.
//
// top is true for the frame where the error was thrown. Every other
// frame's address is a return address: the instruction after the call.
// Looking up address-1 lands on the call itself, which is the line that
// was running, and never on the next line or the next function.
func (s *dartSymbols) lookup(addr uint64, top bool) []location {
	locs, _ := s.find(addr, top)
	return locs
}

// find is lookup that also says whether the address was covered by debug
// info at all. found with no locations means every frame there is a
// hidden compiler helper: drop the frame, as Dart's decoder does, rather
// than keep the raw address and call it missing.
func (s *dartSymbols) find(addr uint64, top bool) (locs []location, found bool) {
	if !top && addr > 0 {
		addr--
	}
	i := sort.Search(len(s.funcs), func(i int) bool { return s.funcs[i].high > addr })
	if i == len(s.funcs) || s.funcs[i].low > addr {
		return nil, false
	}
	fn := s.funcs[i]

	var chain []dartInline // inlined calls covering addr, outermost first
	for _, in := range fn.inlines {
		for _, rg := range in.ranges {
			if addr >= rg[0] && addr < rg[1] {
				chain = append(chain, in)
				break
			}
		}
	}
	sort.SliceStable(chain, func(a, b int) bool { return chain[a].depth < chain[b].depth })

	inner := location{Function: fn.name}
	if row, ok := s.lineAt(addr); ok {
		inner.File, inner.Line, inner.Col = row.file, row.line, row.col
	}
	var files []string
	if fn.cu != nil {
		files = s.files[fn.cu.Offset]
	}
	// The innermost inlined function is where the code actually is. Each
	// level outward is its caller, at the inline's call site. Frames for
	// artificial (compiler-generated) functions are hidden, with the same
	// rules as Dart's decoder: the innermost one unless addr is its very
	// first instruction (static method tear-offs are artificial but should
	// show), and callers whenever they're artificial.
	innerMarks := fn.marks
	if len(chain) > 0 {
		inner.Function = chain[len(chain)-1].name
		innerMarks = chain[len(chain)-1].marks
	}
	var out []location
	if !(innerMarks.artificial && !(innerMarks.hasLowPC && addr == innerMarks.lowPC)) {
		out = append(out, inner)
	}
	for k := len(chain) - 1; k >= 0; k-- {
		caller, callerMarks := fn.name, fn.marks
		if k > 0 {
			caller, callerMarks = chain[k-1].name, chain[k-1].marks
		}
		if callerMarks.artificial {
			continue
		}
		loc := location{Function: caller, Line: int(chain[k].callLine), Col: int(chain[k].callCol)}
		if f := chain[k].callFile; f >= 0 && int(f) < len(files) {
			loc.File = files[f]
		}
		out = append(out, loc)
	}
	return out, true
}

// elfBuildID reads the GNU build-id note: a 4-byte name size, 4-byte
// description size, 4-byte type, the name "GNU\0", then the id bytes.
func elfBuildID(f *elf.File) string {
	for _, sec := range f.Sections {
		if sec.Type != elf.SHT_NOTE {
			continue
		}
		data, err := sec.Data()
		if err != nil {
			continue
		}
		for len(data) >= 12 {
			namesz := f.ByteOrder.Uint32(data[0:4])
			descsz := f.ByteOrder.Uint32(data[4:8])
			typ := f.ByteOrder.Uint32(data[8:12])
			nameEnd := 12 + align4(namesz)
			descEnd := nameEnd + align4(descsz)
			if uint32(len(data)) < descEnd {
				break
			}
			name := string(bytes.TrimRight(data[12:12+namesz], "\x00"))
			if typ == 3 && name == "GNU" { // NT_GNU_BUILD_ID
				return hex.EncodeToString(data[nameEnd : nameEnd+descsz])
			}
			data = data[descEnd:]
		}
	}
	return ""
}

func align4(n uint32) uint32 { return (n + 3) &^ 3 }

// machoUUID reads the LC_UUID load command, which Dart prints as build_id.
func machoUUID(f *macho.File) string {
	const lcUUID = 0x1b
	for _, l := range f.Loads {
		raw := l.Raw()
		if len(raw) >= 24 && f.ByteOrder.Uint32(raw[0:4]) == lcUUID {
			return hex.EncodeToString(raw[8:24])
		}
	}
	return ""
}
