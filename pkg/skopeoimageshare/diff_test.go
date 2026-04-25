package skopeoimageshare

import (
	"sort"
	"testing"
)

func sorted(s DigestSet) []string {
	out := s.Slice()
	sort.Strings(out)
	return out
}

func eq(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestDiff(t *testing.T) {
	t.Parallel()

	const (
		manifest = "sha256:m"
		config   = "sha256:c"
		l1       = "sha256:l1"
		l2       = "sha256:l2"
		l3       = "sha256:l3"
	)

	cases := []struct {
		name   string
		need   DigestSet
		have   DigestSet
		pinned DigestSet
		want   []string
	}{
		{
			name:   "empty have, all sent (incl pinned)",
			need:   NewDigestSet(manifest, config, l1, l2),
			have:   NewDigestSet(),
			pinned: NewDigestSet(manifest, config),
			want:   []string{config, l1, l2, manifest},
		},
		{
			name:   "full overlap, only pinned sent",
			need:   NewDigestSet(manifest, config, l1, l2),
			have:   NewDigestSet(manifest, config, l1, l2),
			pinned: NewDigestSet(manifest, config),
			want:   []string{config, manifest},
		},
		{
			name:   "partial overlap",
			need:   NewDigestSet(manifest, config, l1, l2, l3),
			have:   NewDigestSet(l1, l2),
			pinned: NewDigestSet(manifest, config),
			want:   []string{config, l3, manifest},
		},
		{
			name:   "pinned digest in have is still emitted",
			need:   NewDigestSet(l1),
			have:   NewDigestSet(manifest, config, l1),
			pinned: NewDigestSet(manifest, config),
			want:   []string{config, manifest},
		},
		{
			name:   "no pinned",
			need:   NewDigestSet(l1, l2),
			have:   NewDigestSet(l1),
			pinned: NewDigestSet(),
			want:   []string{l2},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sorted(Diff(tc.need, tc.have, tc.pinned))
			sort.Strings(tc.want)
			if !eq(got, tc.want) {
				t.Errorf("Diff: got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestUnion(t *testing.T) {
	t.Parallel()
	a := NewDigestSet("a", "b")
	b := NewDigestSet("b", "c")
	got := sorted(a.Union(b))
	want := []string{"a", "b", "c"}
	if !eq(got, want) {
		t.Errorf("Union: got %v, want %v", got, want)
	}
}
