package skopeoimageshare

// DigestSet is a set of fully-qualified blob digests
// (i.e. "sha256:<hex>"). Membership testing is the only operation we
// rely on, so map[...]struct{} is the canonical representation.
type DigestSet map[string]struct{}

// NewDigestSet returns a [DigestSet] populated with ds.
func NewDigestSet(ds ...string) DigestSet {
	s := make(DigestSet, len(ds))
	for _, d := range ds {
		s[d] = struct{}{}
	}
	return s
}

// Add adds d to s.
func (s DigestSet) Add(d string) { s[d] = struct{}{} }

// Has reports whether d is in s.
func (s DigestSet) Has(d string) bool { _, ok := s[d]; return ok }

// Union returns s ∪ other (a fresh set; neither input is mutated).
func (s DigestSet) Union(other DigestSet) DigestSet {
	out := make(DigestSet, len(s)+len(other))
	for d := range s {
		out[d] = struct{}{}
	}
	for d := range other {
		out[d] = struct{}{}
	}
	return out
}

// Slice returns the digests in s as a slice in unspecified order.
func (s DigestSet) Slice() []string {
	out := make([]string, 0, len(s))
	for d := range s {
		out = append(out, d)
	}
	return out
}

// Diff computes the set of digests to transfer to the peer.
//
// Membership rule: a digest is in the result iff
//   - it is in pinned (always include — meta-blobs that the peer-side
//     load step assumes are present), or
//   - it is in need but not in have (the layer-deduplication rule).
//
// Pinned digests not appearing in need are still included; this is
// intentional, so callers can pass a pinned set that may overlap need
// or not.
func Diff(need, have, pinned DigestSet) DigestSet {
	out := make(DigestSet, len(need))
	for d := range pinned {
		out[d] = struct{}{}
	}
	for d := range need {
		if _, already := out[d]; already {
			continue
		}
		if _, ok := have[d]; ok {
			continue
		}
		out[d] = struct{}{}
	}
	return out
}
