package docker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
)

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

func readFixture(t *testing.T, name string) []byte {
	t.Helper()
	wd, _ := os.Getwd()
	p := filepath.Join(wd, "..", "..", "..", "internal", "testdata", "dockeroutput", name)
	data, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read fixture %s: %v", name, err)
	}
	return data
}

func TestDocker_Version_TrimsOutput(t *testing.T) {
	t.Parallel()
	r := &stubRunner{out: []byte("Docker version 26.1.3, build abcdef\n")}
	d := NewDocker(r)
	v, err := d.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "Docker version 26.1.3, build abcdef" {
		t.Errorf("Version = %q", v)
	}
}

func TestDocker_ImageLs_Argv(t *testing.T) {
	t.Parallel()
	r := &stubRunner{out: []byte{}}
	d := NewDocker(r)
	if _, err := d.ImageLs(context.Background()); err != nil {
		t.Fatal(err)
	}
	want := [][]string{{"image", "ls", "--format", "json"}}
	if !reflect.DeepEqual(r.got, want) {
		t.Errorf("argv: got %v", r.got)
	}
}

func TestDocker_ImageLs_NDJSON(t *testing.T) {
	t.Parallel()
	in := []byte(`{"ID":"sha256:0b","Repository":"ubuntu","Tag":"noble-20260410","Digest":""}
{"ID":"sha256:0b","Repository":"<none>","Tag":"<none>","Digest":""}
{"ID":"sha256:50","Repository":"plantuml/plantuml-server","Tag":"jetty-v1.2026.2","Digest":""}
`)
	d := NewDocker(&stubRunner{out: in})
	refs, err := d.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"ubuntu:noble-20260410", "plantuml/plantuml-server:jetty-v1.2026.2"}
	if !reflect.DeepEqual(refs, want) {
		t.Errorf("refs: got %v, want %v", refs, want)
	}
}

func TestDocker_ImageLs_Fixture(t *testing.T) {
	t.Parallel()
	d := NewDocker(&stubRunner{out: readFixture(t, "docker-image-ls-json.json")})
	refs, err := d.ImageLs(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(refs, "ubuntu:noble-20260410") {
		t.Errorf("expected fixture to yield 'ubuntu:noble-20260410'; got %v", refs)
	}
}

func TestParseDockerImageLs_Empty(t *testing.T) {
	t.Parallel()
	imgs, err := ParseDockerImageLs(nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) != 0 {
		t.Errorf("expected empty, got %v", imgs)
	}
}

func TestParseDockerImageLs_Dedup(t *testing.T) {
	t.Parallel()
	in := []byte(`{"Repository":"x","Tag":"1"}
{"Repository":"x","Tag":"1"}
{"Repository":"y","Tag":"2"}
`)
	imgs, err := ParseDockerImageLs(in)
	if err != nil {
		t.Fatal(err)
	}
	got := imageRefsFromDockerList(imgs)
	if !reflect.DeepEqual(got, []string{"x:1", "y:2"}) {
		t.Errorf("got %v", got)
	}
}

// TestParseDockerImageInspect_Fixture verifies that the
// internal/testdata/dockeroutput/docker-image-inspect.json sample
// parses cleanly into RepoTags via the alternative parser (the live
// enumeration uses `image ls --format json`).
func TestParseDockerImageInspect_Fixture(t *testing.T) {
	t.Parallel()
	imgs, err := ParseDockerImageInspect(readFixture(t, "docker-image-inspect.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(imgs) == 0 {
		t.Fatal("expected at least one image")
	}
	refs := imageRefsFromDockerInspect(imgs)
	if !slices.Contains(refs, "ubuntu:noble-20260410") {
		t.Errorf("expected ref 'ubuntu:noble-20260410' in %v", refs)
	}
	if imgs[0].Id == "" {
		t.Error("expected Id field populated")
	}
}

func TestDocker_PropagatesRunnerError(t *testing.T) {
	t.Parallel()
	want := errors.New("boom")
	d := NewDocker(&stubRunner{err: want})
	_, err := d.ImageLs(context.Background())
	if !errors.Is(err, want) {
		t.Errorf("got %v, want wrap of %v", err, want)
	}
}
