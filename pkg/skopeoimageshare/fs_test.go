package skopeoimageshare

import (
	"os"
	"path/filepath"
	"testing"
)

func mustFS(t *testing.T) (FS, string) {
	t.Helper()
	dir := t.TempDir()
	fs, err := NewLocalFS(dir)
	if err != nil {
		t.Fatal(err)
	}
	return fs, dir
}

func TestLocalFS_Stat_NotExist(t *testing.T) {
	t.Parallel()
	fs, _ := mustFS(t)
	size, ok, err := statSize(fs, "nope")
	if err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if ok || size != 0 {
		t.Errorf("expected ok=false,size=0, got ok=%v,size=%d", ok, size)
	}
}

func TestLocalFS_Stat_OK(t *testing.T) {
	t.Parallel()
	fs, dir := mustFS(t)
	if err := os.WriteFile(filepath.Join(dir, "f"), []byte("abc"), 0o644); err != nil {
		t.Fatal(err)
	}
	size, ok, err := statSize(fs, "f")
	if err != nil {
		t.Fatal(err)
	}
	if !ok || size != 3 {
		t.Errorf("got ok=%v,size=%d, want true,3", ok, size)
	}
}

func TestLocalFS_RemoveENOENTSafe(t *testing.T) {
	t.Parallel()
	fs, _ := mustFS(t)
	if err := fs.Remove("missing"); err != nil && !os.IsNotExist(err) {
		t.Errorf("Remove of missing should be ENOENT-safe, got %v", err)
	}
}

func TestSafeWrite_AtomicAndIdempotent(t *testing.T) {
	t.Parallel()
	fs, dir := mustFS(t)
	rel := filepath.Join("sub", "x.json")

	if err := SafeWrite(fs, rel, []byte("{}")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "{}" {
		t.Errorf("got %q", got)
	}

	if err := SafeWrite(fs, rel, []byte(`{"k":1}`)); err != nil {
		t.Fatal(err)
	}
	got, err = os.ReadFile(filepath.Join(dir, rel))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"k":1}` {
		t.Errorf("after rewrite got %q", got)
	}

	entries, _ := os.ReadDir(filepath.Join(dir, "sub"))
	for _, e := range entries {
		if e.Name() != "x.json" {
			t.Errorf("unexpected leftover: %s", e.Name())
		}
	}
}
