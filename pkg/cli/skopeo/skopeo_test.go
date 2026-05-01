package skopeo

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
)

// stubRunner records argvs it was called with and returns canned
// output / error.
type stubRunner struct {
	got [][]string
	out []byte
	err error
}

func (r *stubRunner) Run(ctx context.Context, argv []string) ([]byte, error) {
	dup := make([]string, len(argv))
	copy(dup, argv)
	r.got = append(r.got, dup)
	return r.out, r.err
}

func TestSkopeo_Version_TrimsOutput(t *testing.T) {
	t.Parallel()
	r := &stubRunner{out: []byte("skopeo version 1.20.0\n")}
	s := New(r)
	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "skopeo version 1.20.0" {
		t.Errorf("Version = %q", v)
	}
	if !reflect.DeepEqual(r.got, [][]string{{"--version"}}) {
		t.Errorf("argv: got %v", r.got)
	}
}

func TestSkopeo_InspectRaw_Argv(t *testing.T) {
	t.Parallel()
	r := &stubRunner{out: []byte(`{"schemaVersion":2}`)}
	s := New(r)
	_, err := s.InspectRaw(context.Background(), "containers-storage", "myimg:latest")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"inspect", "--raw", "containers-storage:myimg:latest"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_InspectRawShared_Argv(t *testing.T) {
	t.Parallel()
	r := &stubRunner{}
	s := New(r)
	_, err := s.InspectRawShared(context.Background(), "/tmp/oci/_tags/v1", "ghcr.io/a/b:c", "/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"inspect", "--raw", "--shared-blob-dir", "/tmp/share", "oci:/tmp/oci/_tags/v1:ghcr.io/a/b:c"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v, want %v", r.got, want)
	}
}

func TestSkopeo_InspectRawShared_RejectsEmptyDirOrRef(t *testing.T) {
	t.Parallel()
	s := New(&stubRunner{})
	if _, err := s.InspectRawShared(context.Background(), "", "x:y", "/share"); err == nil {
		t.Error("expected error for empty ociDir")
	}
	if _, err := s.InspectRawShared(context.Background(), "/dir", "", "/share"); err == nil {
		t.Error("expected error for empty imageRef")
	}
}

func TestSkopeo_CopyToOCI_Argv(t *testing.T) {
	t.Parallel()
	r := &stubRunner{}
	s := New(r)
	err := s.CopyToOCI(context.Background(),
		"containers-storage", "ghcr.io/a/b:c",
		"/tmp/oci/_tags/c", "ghcr.io/a/b:c", "/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"copy",
		"--dest-shared-blob-dir", "/tmp/share",
		"containers-storage:ghcr.io/a/b:c",
		"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
	}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_CopyFromOCI_Argv(t *testing.T) {
	t.Parallel()
	r := &stubRunner{}
	s := New(r)
	err := s.CopyFromOCI(context.Background(),
		"/tmp/oci/_tags/c", "ghcr.io/a/b:c", "/tmp/share",
		"containers-storage", "ghcr.io/a/b:c")
	if err != nil {
		t.Fatal(err)
	}
	want := [][]string{{
		"copy",
		"--src-shared-blob-dir", "/tmp/share",
		"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
		"containers-storage:ghcr.io/a/b:c",
	}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_CopyToOCI_RejectsEmptyDirOrRef(t *testing.T) {
	t.Parallel()
	s := New(&stubRunner{})
	if err := s.CopyToOCI(context.Background(), "containers-storage", "x:y", "", "x:y", "/share"); err == nil {
		t.Error("expected error for empty ociDir")
	}
	if err := s.CopyToOCI(context.Background(), "containers-storage", "x:y", "/dir", "", "/share"); err == nil {
		t.Error("expected error for empty imageRef")
	}
}

func TestSkopeo_CopyFromOCI_RejectsEmptyDirOrRef(t *testing.T) {
	t.Parallel()
	s := New(&stubRunner{})
	if err := s.CopyFromOCI(context.Background(), "", "x:y", "/share", "containers-storage", "x:y"); err == nil {
		t.Error("expected error for empty ociDir")
	}
	if err := s.CopyFromOCI(context.Background(), "/dir", "", "/share", "containers-storage", "x:y"); err == nil {
		t.Error("expected error for empty imageRef")
	}
}

func TestSkopeo_CompressionArgs(t *testing.T) {
	t.Parallel()
	r := &stubRunner{}
	s := New(r)
	s.CompressionFormat = "zstd"
	s.CompressionLevel = 19
	if err := s.CopyToOCI(context.Background(),
		"containers-storage", "ghcr.io/a/b:c",
		"/tmp/oci/_tags/c", "ghcr.io/a/b:c", "/tmp/share"); err != nil {
		t.Fatal(err)
	}
	if err := s.CopyFromOCI(context.Background(),
		"/tmp/oci/_tags/c", "ghcr.io/a/b:c", "/tmp/share",
		"containers-storage", "ghcr.io/a/b:c"); err != nil {
		t.Fatal(err)
	}
	want := [][]string{
		{
			"copy",
			"--dest-compress-format", "zstd",
			"--dest-compress-level", "19",
			"--dest-shared-blob-dir", "/tmp/share",
			"containers-storage:ghcr.io/a/b:c",
			"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
		},
		{
			"copy",
			"--dest-compress-format", "zstd",
			"--dest-compress-level", "19",
			"--src-shared-blob-dir", "/tmp/share",
			"oci:/tmp/oci/_tags/c:ghcr.io/a/b:c",
			"containers-storage:ghcr.io/a/b:c",
		},
	}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv mismatch:\n got: %v\nwant: %v", r.got, want)
	}
}

func TestSkopeo_PropagatesRunnerError(t *testing.T) {
	t.Parallel()
	want := errors.New("simulated runner error")
	r := &stubRunner{err: want}
	s := New(r)
	_, err := s.InspectRaw(context.Background(), "containers-storage", "x:y")
	if !errors.Is(err, want) {
		t.Errorf("expected wrapped %v, got %v", want, err)
	}
	if !strings.Contains(err.Error(), "simulated runner error") {
		t.Errorf("error text = %q", err.Error())
	}
}
