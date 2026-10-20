package symbolicate

import (
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Real debug files and traces, with what Dart's own decoder
// (package:native_stack_traces) printed for each trace. Our output must
// match the official one frame for frame. testdata/README.md says how
// each was made.
var dartFixtures = []struct {
	name, debug, trace, decoded, buildID string
}{
	{"Dart AOT on macOS (Mach-O)", "dart_macos_arm64.debug", "dart_macos_arm64.trace.txt", "dart_macos_arm64.decoded.txt", "b4485c2e9c56418697e466d0c2ca3195"},
	// From an obfuscated Flutter release build on the Android emulator. Its
	// line table has sequences out of address order, which broke a
	// forward-scanning lookup.
	{"Flutter on Android (ELF)", "flutter_android_arm64.symbols", "flutter_android_arm64.trace.txt", "flutter_android_arm64.decoded.txt", "b718850940a6ab861ab806427ac43cab"},
}

func TestDartSymbolsMatchDartsOwnDecoder(t *testing.T) {
	for _, fx := range dartFixtures {
		t.Run(fx.name, func(t *testing.T) {
			data, err := os.ReadFile("testdata/" + fx.debug)
			if err != nil {
				t.Fatal(err)
			}
			syms, err := parseDartSymbols(data)
			if err != nil {
				t.Fatal(err)
			}
			if syms.buildID != fx.buildID {
				t.Fatalf("build id = %s, want %s", syms.buildID, fx.buildID)
			}
			trace, _ := os.ReadFile("testdata/" + fx.trace)
			var got []string
			frame := regexp.MustCompile(`#(\d+) abs \S+ virt \S+ (\S+)`)
			for _, m := range frame.FindAllStringSubmatch(string(trace), -1) {
				addr, ok := syms.address(m[2])
				if !ok {
					t.Fatalf("could not resolve %s", m[2])
				}
				// "#00" starts each trace (a file can hold several); it's
				// the only frame that isn't a return address.
				for _, loc := range syms.lookup(addr, m[1] == "00") {
					got = append(got, describe(loc))
				}
			}
			want := officialFrames(t, fx.decoded)
			if strings.Join(got, "\n") != strings.Join(want, "\n") {
				t.Fatalf("frames differ from Dart's decoder\n got:\n  %s\nwant:\n  %s", strings.Join(got, "\n  "), strings.Join(want, "\n  "))
			}
		})
	}
}

func TestDartAddressForms(t *testing.T) {
	s := &dartSymbols{instructions: map[string]uint64{"_kDartIsolateSnapshotInstructions": 0x10000}}
	for in, want := range map[string]uint64{
		"_kDartIsolateSnapshotInstructions+0x34c67": 0x44c67,
		"0x44c67": 0x44c67, // a bare virtual address
	} {
		if got, ok := s.address(in); !ok || got != want {
			t.Errorf("address(%q) = %#x, %v; want %#x", in, got, ok, want)
		}
	}
	if _, ok := s.address("_kDartVmSnapshotInstructions+0x10"); ok {
		t.Error("unknown symbol should not resolve")
	}
}

func TestDartRejectsOtherFiles(t *testing.T) {
	if _, err := parseDartSymbols([]byte("{\"version\":3}")); err == nil {
		t.Fatal("a source map is not a Dart debug file")
	}
}

// "CartPage.total (file:///shop/bin/shop.dart:11:76)" from the official
// output, reduced to "CartPage.total shop.dart:11:76".
func officialFrames(t *testing.T, name string) []string {
	t.Helper()
	data, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`^#\d+\s+(.+) \((.+):(\d+):(\d+)\)$`)
	var out []string
	for _, line := range strings.Split(string(data), "\n") {
		if m := re.FindStringSubmatch(line); m != nil {
			out = append(out, m[1]+" "+path.Base(m[2])+":"+m[3]+":"+m[4])
		}
	}
	if len(out) == 0 {
		t.Fatal("no frames in the official output")
	}
	return out
}

func describe(l location) string {
	return l.Function + " " + path.Base(l.File) + ":" + itoa(l.Line) + ":" + itoa(l.Col)
}

func itoa(n int) string { return strconv.Itoa(n) }

// Every frame but the top one holds a return address: the instruction
// after the call. When a call is the last instruction of a line, that
// address already belongs to the next line, so it must be looked up as
// address-1. This finds such a boundary in the real debug file and checks
// both cases.
func TestReturnAddressesResolveToTheCallLine(t *testing.T) {
	data, _ := os.ReadFile("testdata/dart_macos_arm64.debug")
	syms, err := parseDartSymbols(data)
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, fn := range syms.funcs {
		if len(fn.inlines) > 0 {
			continue
		}
		for i := 1; i < len(syms.rows) && checked < 20; i++ {
			prev, cur := syms.rows[i-1], syms.rows[i]
			// A new line starts at cur.addr, inside this function, and the
			// byte before it belongs to a different line.
			if cur.addr <= fn.low || cur.addr >= fn.high || prev.addr < fn.low || prev.end || cur.end || cur.line == prev.line || cur.addr == prev.addr {
				continue
			}
			top := syms.lookup(cur.addr, true)
			ret := syms.lookup(cur.addr, false)
			if top[0].Line != cur.line {
				t.Fatalf("top frame at %#x: line %d, want %d", cur.addr, top[0].Line, cur.line)
			}
			if ret[0].Line != prev.line {
				t.Fatalf("return address %#x: line %d, want the call's line %d", cur.addr, ret[0].Line, prev.line)
			}
			checked++
		}
	}
	if checked == 0 {
		t.Fatal("found no line boundary to check")
	}
}

// Real Flutter symbol files have line tables whose sequences aren't in
// address order. Looking up addresses in descending order (the opposite
// of how a forward-scanning reader likes them) must still work.
func TestLookupsInAnyOrder(t *testing.T) {
	data, _ := os.ReadFile("testdata/dart_macos_arm64.debug")
	syms, _ := parseDartSymbols(data)
	addrs := []uint64{0x5012a, 0x501e6, 0x44c67}
	var lines []int
	for _, a := range addrs {
		lines = append(lines, syms.lookup(a, true)[0].Line)
	}
	for k := len(addrs) - 1; k >= 0; k-- {
		if got := syms.lookup(addrs[k], true)[0].Line; got != lines[k] || got == 0 {
			t.Fatalf("%#x: line %d, then %d", addrs[k], lines[k], got)
		}
	}
}
