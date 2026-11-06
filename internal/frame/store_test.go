package frame

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

const id1 = "0123456789abcdef0123456789abcdef"

func TestStorePutReplaceDelete(t *testing.T) {
	dir := t.TempDir()
	s, err := OpenStore(dir, testLog)
	if err != nil {
		t.Fatal(err)
	}
	var mu sync.Mutex
	var added, removed []string
	s.SetOnChange(func(a, r []string) {
		mu.Lock()
		added, removed = append(added, a...), append(removed, r...)
		mu.Unlock()
	})

	jpg := append([]byte{0xFF, 0xD8}, bytes.Repeat([]byte{1}, 100)...)
	if err := s.Put(id1, "100x100v1", bytes.NewReader(jpg), 1<<20); err != nil {
		t.Fatal(err)
	}
	if err := s.Put(id1, "200x200v1", bytes.NewReader(jpg), 1<<20); err != nil {
		t.Fatal(err)
	}
	files, _ := os.ReadDir(dir)
	if len(files) != 1 || files[0].Name() != id1+"_200x200v1.jpg" {
		t.Fatalf("expected only the new rendition, got %v", files)
	}
	if m := s.Manifest(); len(m) != 1 || m[0].Rev != "200x200v1" || m[0].Size != 102 {
		t.Fatalf("manifest: %+v", m)
	}
	if len(added) != 1 {
		t.Fatalf("replacing a rendition must not re-announce the photo: %v", added)
	}

	if err := s.Put(id1, "x", strings.NewReader("GIF89a"), 1<<20); err != ErrNotJPEG {
		t.Fatalf("expected ErrNotJPEG, got %v", err)
	}
	if err := s.Put(id1, "x", bytes.NewReader(jpg), 10); err == nil {
		t.Fatal("expected size limit error")
	}
	if tmps, _ := filepath.Glob(filepath.Join(dir, ".upload-*")); len(tmps) != 0 {
		t.Fatalf("temp files left behind: %v", tmps)
	}

	if found, err := s.Delete(id1); !found || err != nil {
		t.Fatalf("delete: %v %v", found, err)
	}
	if len(removed) != 1 || s.Count() != 0 {
		t.Fatalf("after delete: removed=%v count=%d", removed, s.Count())
	}
}

func TestStoreScanPicksUpManualFiles(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "holiday.JPG"), []byte{0xFF, 0xD8}, 0o644)
	os.WriteFile(filepath.Join(dir, "notes.txt"), []byte("x"), 0o644)
	os.WriteFile(filepath.Join(dir, id1+"_1x1v1.jpg"), []byte{0xFF, 0xD8}, 0o644)
	s, err := OpenStore(dir, testLog)
	if err != nil {
		t.Fatal(err)
	}
	if got := s.Keys(); len(got) != 2 {
		t.Fatalf("keys: %v", got)
	}
	if m := s.Manifest(); len(m) != 1 || m[0].ID != id1 {
		t.Fatalf("manual files must not appear in the manifest: %+v", m)
	}
}
