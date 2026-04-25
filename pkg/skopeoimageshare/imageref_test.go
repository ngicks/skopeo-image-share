package skopeoimageshare

import (
	"strings"
	"testing"
)

func TestParseImageRef(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name     string
		in       string
		wantHost string
		wantPath string
		wantTag  string
		wantDig  string
		wantStr  string
	}{
		{
			name:     "docker hub library implicit",
			in:       "nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub default tag",
			in:       "nginx",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub explicit",
			in:       "docker.io/library/nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "docker hub explicit with single segment",
			in:       "docker.io/nginx:latest",
			wantHost: "docker.io",
			wantPath: "library/nginx",
			wantTag:  "latest",
			wantStr:  "docker.io/library/nginx:latest",
		},
		{
			name:     "ghcr 3 segments",
			in:       "ghcr.io/a/b/c:d",
			wantHost: "ghcr.io",
			wantPath: "a/b/c",
			wantTag:  "d",
			wantStr:  "ghcr.io/a/b/c:d",
		},
		{
			name:     "ghcr 4 segments overlapping prefix",
			in:       "ghcr.io/a/b/c/d:latest",
			wantHost: "ghcr.io",
			wantPath: "a/b/c/d",
			wantTag:  "latest",
			wantStr:  "ghcr.io/a/b/c/d:latest",
		},
		{
			name:     "registry with port",
			in:       "registry.example.com:5000/team/proj/sub/app:tag",
			wantHost: "registry.example.com:5000",
			wantPath: "team/proj/sub/app",
			wantTag:  "tag",
			wantStr:  "registry.example.com:5000/team/proj/sub/app:tag",
		},
		{
			name:     "localhost",
			in:       "localhost/devenv/devenv:0.0.61",
			wantHost: "localhost",
			wantPath: "devenv/devenv",
			wantTag:  "0.0.61",
			wantStr:  "localhost/devenv/devenv:0.0.61",
		},
		{
			name:     "digest pinned",
			in:       "ghcr.io/a/b@sha256:" + strings.Repeat("a", 64),
			wantHost: "ghcr.io",
			wantPath: "a/b",
			wantDig:  strings.Repeat("a", 64),
			wantStr:  "ghcr.io/a/b@sha256:" + strings.Repeat("a", 64),
		},
		{
			name:     "digest pinned with port",
			in:       "registry.example.com:5000/x@sha256:" + strings.Repeat("0", 64),
			wantHost: "registry.example.com:5000",
			wantPath: "x", // no library canonicalization on non-docker.io
			wantDig:  strings.Repeat("0", 64),
			wantStr:  "registry.example.com:5000/x@sha256:" + strings.Repeat("0", 64),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := ParseImageRef(tc.in)
			if err != nil {
				t.Fatalf("ParseImageRef(%q) unexpected error: %v", tc.in, err)
			}
			if got.Host != tc.wantHost {
				t.Errorf("Host: got %q, want %q", got.Host, tc.wantHost)
			}
			if got.Path != tc.wantPath {
				t.Errorf("Path: got %q, want %q", got.Path, tc.wantPath)
			}
			if got.Tag != tc.wantTag {
				t.Errorf("Tag: got %q, want %q", got.Tag, tc.wantTag)
			}
			if got.Digest != tc.wantDig {
				t.Errorf("Digest: got %q, want %q", got.Digest, tc.wantDig)
			}
			if got.String() != tc.wantStr {
				t.Errorf("String: got %q, want %q", got.String(), tc.wantStr)
			}
			if got.Original != tc.in {
				t.Errorf("Original: got %q, want %q", got.Original, tc.in)
			}
		})
	}
}

func TestParseImageRef_Errors(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", "empty"},
		{"empty tag", "nginx:", "empty tag"},
		{"reserved _tags", "ghcr.io/a/_tags/b:1", "reserved segment"},
		{"reserved _digests", "ghcr.io/a/_digests/b:1", "reserved segment"},
		{"digest missing prefix", "ghcr.io/a/b@deadbeef", `must start with "sha256:"`},
		{"digest short hex", "ghcr.io/a/b@sha256:abc", "expected 64 hex"},
		{"digest non-hex", "ghcr.io/a/b@sha256:" + strings.Repeat("z", 64), "non-hex"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseImageRef(tc.in)
			if err == nil {
				t.Fatalf("ParseImageRef(%q) expected error containing %q, got nil", tc.in, tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("ParseImageRef(%q) error = %v, want substring %q", tc.in, err, tc.want)
			}
		})
	}
}
