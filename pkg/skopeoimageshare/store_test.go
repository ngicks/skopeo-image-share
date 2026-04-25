package skopeoimageshare

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestStorePaths(t *testing.T) {
	t.Parallel()
	base := "/tmp/x"
	st := NewStore(base)

	tagged, err := ParseImageRef("ghcr.io/a/b/c:d")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := st.TagDir(tagged), filepath.Join(base, "ghcr.io", "a", "b", "c", "_tags", "d"); got != want {
		t.Errorf("TagDir: got %q, want %q", got, want)
	}
	if got := st.DigestDir(tagged); got != "" {
		t.Errorf("DigestDir on tagged ref: got %q, want empty", got)
	}

	digested, err := ParseImageRef("ghcr.io/x/y@sha256:" + strings.Repeat("0", 64))
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(base, "ghcr.io", "x", "y", "_digests", strings.Repeat("0", 64))
	if got := st.DigestDir(digested); got != want {
		t.Errorf("DigestDir: got %q, want %q", got, want)
	}
	if got := st.TagDir(digested); got != "" {
		t.Errorf("TagDir on digested ref: got %q, want empty", got)
	}

	if got, want := st.ShareDir(), filepath.Join(base, "share"); got != want {
		t.Errorf("ShareDir: got %q, want %q", got, want)
	}
	if got, want := st.TmpDir(), filepath.Join(base, "tmp"); got != want {
		t.Errorf("TmpDir: got %q, want %q", got, want)
	}
	if got, want := st.LogDir(), filepath.Join(base, "log"); got != want {
		t.Errorf("LogDir: got %q, want %q", got, want)
	}
}

func TestStoreDumpDir(t *testing.T) {
	t.Parallel()
	st := NewStore("/b")

	tagged, _ := ParseImageRef("nginx:latest")
	if d, err := st.DumpDir(tagged); err != nil || !strings.Contains(d, "_tags") {
		t.Errorf("tagged DumpDir = %q, %v", d, err)
	}

	digested, _ := ParseImageRef("nginx@sha256:" + strings.Repeat("a", 64))
	if d, err := st.DumpDir(digested); err != nil || !strings.Contains(d, "_digests") {
		t.Errorf("digested DumpDir = %q, %v", d, err)
	}
}

func TestStoreEnsureLayoutIdempotent(t *testing.T) {
	t.Parallel()
	st := NewStore(t.TempDir())
	ctx := context.Background()

	for i := range 2 {
		if err := st.EnsureLayout(ctx); err != nil {
			t.Fatalf("EnsureLayout pass %d: %v", i, err)
		}
	}
	for _, d := range []string{st.ShareDir(), st.TmpDir(), st.LogDir()} {
		if fi, err := os.Stat(d); err != nil || !fi.IsDir() {
			t.Errorf("dir %q: stat=%v, isDir=%v", d, err, fi != nil && fi.IsDir())
		}
	}
}

func TestDefaultBaseDir(t *testing.T) {
	t.Setenv("XDG_DATA_HOME", "/custom/xdg")
	got, err := DefaultBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/custom/xdg", AppDirName); got != want {
		t.Errorf("DefaultBaseDir w/ XDG: got %q, want %q", got, want)
	}

	t.Setenv("XDG_DATA_HOME", "")
	t.Setenv("HOME", "/h")
	got, err = DefaultBaseDir()
	if err != nil {
		t.Fatal(err)
	}
	if want := filepath.Join("/h", ".local", "share", AppDirName); got != want {
		t.Errorf("DefaultBaseDir w/o XDG: got %q, want %q", got, want)
	}
}

func TestPosixPaths(t *testing.T) {
	t.Parallel()
	got := PosixTagPath("/r", "ghcr.io", "a/b/c", "d")
	if want := "/r/ghcr.io/a/b/c/_tags/d"; got != want {
		t.Errorf("PosixTagPath: got %q, want %q", got, want)
	}
	got = PosixDigestPath("/r", "ghcr.io", "x", strings.Repeat("a", 64))
	if want := "/r/ghcr.io/x/_digests/" + strings.Repeat("a", 64); got != want {
		t.Errorf("PosixDigestPath: got %q, want %q", got, want)
	}
	if got, want := PosixSharePath("/r"), "/r/share"; got != want {
		t.Errorf("PosixSharePath: got %q, want %q", got, want)
	}
}
