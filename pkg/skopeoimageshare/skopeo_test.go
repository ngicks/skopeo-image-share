package skopeoimageshare

import (
	"context"
	"strings"
	"testing"
)

func TestSkopeo_Version(t *testing.T) {
	writeShim(t, "skopeo", `echo "skopeo version 1.20.0"`)
	s := NewSkopeo(NewLocalRunner("skopeo"))
	v, err := s.Version(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if v != "skopeo version 1.20.0" {
		t.Errorf("version = %q", v)
	}
}

func TestSkopeo_InspectRaw_ArgvAndOutput(t *testing.T) {
	writeShim(t, "skopeo", `
echo "$@" >"$0.argv"
cat <<'EOF'
{"schemaVersion":2,"config":{"digest":"sha256:abc"}}
EOF
`)
	s := NewSkopeo(NewLocalRunner("skopeo"))
	out, err := s.InspectRaw(context.Background(), "containers-storage", "myimg:latest")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(out), "sha256:abc") {
		t.Errorf("output missing digest: %q", out)
	}
}

func TestSkopeo_CopyToOCI_AssemblesArgv(t *testing.T) {
	dir := writeShim(t, "skopeo", `
printf "%s\n" "$@" >"$0.argv"
exit 0
`)
	s := NewSkopeo(NewLocalRunner("skopeo"))
	err := s.CopyToOCI(context.Background(),
		"containers-storage", "ghcr.io/a/b:c",
		"/tmp/oci/_tags/c", "/tmp/share")
	if err != nil {
		t.Fatal(err)
	}
	argvBytes, err := readFile(dir + "/skopeo.argv")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.Split(strings.TrimRight(string(argvBytes), "\n"), "\n")
	want := []string{
		"copy", "--preserve-digests",
		"--dest-shared-blob-dir", "/tmp/share",
		"containers-storage:ghcr.io/a/b:c",
		"oci:/tmp/oci/_tags/c",
	}
	if len(got) != len(want) {
		t.Fatalf("argv len: got %d (%q), want %d (%q)", len(got), got, len(want), want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("argv[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestSkopeo_CopyFromOCI_AssemblesArgv(t *testing.T) {
	dir := writeShim(t, "skopeo", `
printf "%s\n" "$@" >"$0.argv"
exit 0
`)
	s := NewSkopeo(NewLocalRunner("skopeo"))
	err := s.CopyFromOCI(context.Background(),
		"/tmp/oci/_tags/c", "/tmp/share",
		"containers-storage", "ghcr.io/a/b:c")
	if err != nil {
		t.Fatal(err)
	}
	argv, err := readFile(dir + "/skopeo.argv")
	if err != nil {
		t.Fatal(err)
	}
	got := strings.TrimRight(string(argv), "\n")
	want := strings.Join([]string{
		"copy", "--preserve-digests",
		"--src-shared-blob-dir", "/tmp/share",
		"oci:/tmp/oci/_tags/c",
		"containers-storage:ghcr.io/a/b:c",
	}, "\n")
	if got != want {
		t.Errorf("argv:\n got: %q\nwant: %q", got, want)
	}
}

func TestSkopeo_Error_Wraps(t *testing.T) {
	writeShim(t, "skopeo", `echo "broken" >&2; exit 3`)
	s := NewSkopeo(NewLocalRunner("skopeo"))
	_, err := s.InspectRaw(context.Background(), "containers-storage", "x:y")
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "exit 3") {
		t.Errorf("error does not include exit code: %v", err)
	}
}

func readFile(p string) ([]byte, error) {
	return readFileImpl(p)
}
