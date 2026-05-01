package skopeoimageshare

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/ngicks/go-fsys-helper/stream"
)

const blobBody = "hello world\n"

func writeFile(t *testing.T, p string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func makeFSPair(t *testing.T) (srcFS, dstFS Fs, srcRoot, dstRoot string) {
	t.Helper()
	srcRoot = t.TempDir()
	dstRoot = t.TempDir()
	var err error
	srcFS, err = NewLocalFs(srcRoot)
	if err != nil {
		t.Fatal(err)
	}
	dstFS, err = NewLocalFs(dstRoot)
	if err != nil {
		t.Fatal(err)
	}
	return
}

func TestTransferBlob_FreshCopy(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(blobBody))

	if err := TransferBlob(context.Background(), srcFS, "blob", dstFS, "blob", int64(len(blobBody))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != blobBody {
		t.Errorf("got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "blob.part")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part should be gone after success: stat err=%v", err)
	}
}

func TestTransferBlob_ShortCircuitOnExisting(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(blobBody))
	writeFile(t, filepath.Join(dstRoot, "blob"), []byte(blobBody))

	srcStat0, _ := os.Stat(filepath.Join(srcRoot, "blob"))

	if err := TransferBlob(context.Background(), srcFS, "blob", dstFS, "blob", int64(len(blobBody))); err != nil {
		t.Fatal(err)
	}
	srcStat1, _ := os.Stat(filepath.Join(srcRoot, "blob"))
	if srcStat1.ModTime() != srcStat0.ModTime() {
		t.Errorf("src was touched on short-circuit path")
	}
}

func TestTransferBlob_ResumeFromPart(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	const body = "0123456789abcdef"
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(body))
	writeFile(t, filepath.Join(dstRoot, "blob.part"), []byte("0123456"))

	if err := TransferBlob(context.Background(), srcFS, "blob", dstFS, "blob", int64(len(body))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("after resume got %q, want %q", got, body)
	}
}

func TestTransferBlob_PartLargerThanExpected_Restarts(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	const body = "0123456789"
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(body))
	writeFile(t, filepath.Join(dstRoot, "blob.part"), []byte("CORRUPTLONGERTHANEXPECTED"))

	if err := TransferBlob(context.Background(), srcFS, "blob", dstFS, "blob", int64(len(body))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q, want %q", got, body)
	}
}

func TestTransferBlob_PartMatchesExpected_JustRenames(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	const body = "0123456789"
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(body))
	writeFile(t, filepath.Join(dstRoot, "blob.part"), []byte(body))

	if err := TransferBlob(context.Background(), srcFS, "blob", dstFS, "blob", int64(len(body))); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(filepath.Join(dstRoot, "blob"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Errorf("got %q", got)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "blob.part")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part should be gone")
	}
}

func TestTransferBlob_CancellationDuringCopy(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)

	const body = "abcdefghijklmnopqrstuvwxyz"
	writeFile(t, filepath.Join(srcRoot, "blob"), []byte(body))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := TransferBlob(ctx, srcFS, "blob", dstFS, "blob", int64(len(body)))
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("got %v, want context.Canceled", err)
	}
	if _, err := os.Stat(filepath.Join(dstRoot, "blob")); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("dst should not exist after cancel: %v", err)
	}
}

func TestCopyTagDirSmallFiles(t *testing.T) {
	t.Parallel()
	srcFS, dstFS, srcRoot, dstRoot := makeFSPair(t)
	writeFile(t, filepath.Join(srcRoot, "_tags", "v1", "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`))
	writeFile(t, filepath.Join(srcRoot, "_tags", "v1", "index.json"), []byte(`{"schemaVersion":2,"manifests":[]}`))

	err := CopyTagDirSmallFiles(context.Background(),
		srcFS, "_tags/v1", dstFS, "_tags/v1",
		[]string{"oci-layout", "index.json"})
	if err != nil {
		t.Fatal(err)
	}

	for _, n := range []string{"oci-layout", "index.json"} {
		got, err := os.ReadFile(filepath.Join(dstRoot, "_tags", "v1", n))
		if err != nil {
			t.Fatalf("missing %s: %v", n, err)
		}
		if len(got) == 0 {
			t.Errorf("%s empty", n)
		}
	}
}

// TestStreamCancellable_UnblocksRead verifies the cancellable-Read seam
// (PLAN §8 / TODO 3.5).
func TestStreamCancellable_UnblocksRead(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())

	br := &blockingReader{}
	r := stream.NewCancellable(ctx, br)

	buf := make([]byte, 1)
	if n, err := r.Read(buf); n != 1 || err != nil {
		t.Fatalf("first read: n=%d err=%v, want 1, nil", n, err)
	}
	cancel()
	if _, err := r.Read(buf); !errors.Is(err, context.Canceled) {
		t.Fatalf("second read after cancel: err=%v, want context.Canceled", err)
	}
}

type blockingReader struct{}

func (br *blockingReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, io.ErrShortBuffer
	}
	p[0] = 'X'
	return 1, nil
}
