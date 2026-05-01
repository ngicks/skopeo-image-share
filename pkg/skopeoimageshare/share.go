package skopeoimageshare

import "context"

// Share bundles a [*Local] with a [Remote] peer and exposes thin
// convenience wrappers around the underlying [Local.Push] and
// [Local.Pull] methods. Closing a [Share] forwards to [Remote.Close];
// [*Local] has no resources to release.
type Share struct {
	Local  *Local
	Remote Remote
}

// NewShare returns a [*Share] wrapping local and remote.
func NewShare(local *Local, remote Remote) *Share {
	return &Share{Local: local, Remote: remote}
}

// Close forwards to [Remote.Close].
func (s *Share) Close() error {
	if s.Remote == nil {
		return nil
	}
	return s.Remote.Close()
}

// Push is the thin wrapper around [Local.Push]: pushes args.Images
// from Local to Remote.
func (s *Share) Push(ctx context.Context, args PushArgs) (PushResult, error) {
	return s.Local.Push(ctx, args, s.Remote)
}

// Pull is the thin wrapper around [Local.Pull]: pulls args.Images
// from Remote to Local.
func (s *Share) Pull(ctx context.Context, args PullArgs) (PullResult, error) {
	return s.Local.Pull(ctx, args, s.Remote)
}
