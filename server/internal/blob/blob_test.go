package blob

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPutAndOpen(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	b, err := s.Put(strings.NewReader("hello"), 100)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello"), a known value, so the hash isn't just "some string"
	if b.SHA256 != "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824" || b.Size != 5 {
		t.Fatalf("blob = %+v", b)
	}
	f, err := s.Open(b.SHA256)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	got, _ := io.ReadAll(f)
	if string(got) != "hello" {
		t.Fatalf("read back %q", got)
	}
}

func TestSameContentStoredOnce(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	a, _ := s.Put(strings.NewReader("same"), 100)
	b, _ := s.Put(strings.NewReader("same"), 100)
	if a.SHA256 != b.SHA256 {
		t.Fatal("same bytes, different hashes")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "objects", a.SHA256[:2]))
	if len(entries) != 1 {
		t.Fatalf("%d objects stored, want 1", len(entries))
	}
}

func TestPutRefusesOversizedFilesAndLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	s, _ := New(dir)
	_, err := s.Put(bytes.NewReader(make([]byte, 101)), 100)
	if !errors.Is(err, ErrTooLarge) {
		t.Fatalf("err = %v, want ErrTooLarge", err)
	}
	if tmp, _ := os.ReadDir(filepath.Join(dir, "tmp")); len(tmp) != 0 {
		t.Fatalf("left %d temp files behind", len(tmp))
	}
	if _, err := s.Put(bytes.NewReader(make([]byte, 100)), 100); err != nil {
		t.Fatalf("exactly the limit should be accepted: %v", err)
	}
}

func TestOpenRejectsPathTricks(t *testing.T) {
	s, _ := New(t.TempDir())
	if _, err := s.Open("../../etc/passwd"); err == nil {
		t.Fatal("a non-hash name must be rejected")
	}
}
