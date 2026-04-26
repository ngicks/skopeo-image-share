package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"path"
	"sort"
	"time"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/skopeo-image-share/pkg/cli"
)

// PushArgs configures one [Push] invocation. Flags surfaced via the
// CLI (`cmd/skopeo-image-share/commands/push.go`) map 1:1 to fields on
// this struct; keep the cobra side a translation layer only.
type PushArgs struct {
	// Images is the list of refs to push (e.g. "ghcr.io/a/b:c").
	Images []string

	// LocalTransport is "containers-storage", "docker-daemon", or "oci".
	LocalTransport string
	// LocalPath is required when LocalTransport == "oci"; otherwise unused.
	LocalPath string

	// RemoteTransport mirrors LocalTransport on the peer side.
	RemoteTransport string
	// RemotePath is required when RemoteTransport == "oci".
	RemotePath string

	// DataDir overrides the local app data dir (defaults to
	// `${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share`).
	DataDir string

	// Jobs is per-blob parallelism; 0 → 4.
	Jobs int

	// DryRun replaces all mutating operations (local dump, network
	// transfer, peer load) with read-only equivalents and emits a plan
	// instead of state changes.
	DryRun bool

	// AssumeRemoteHas is a literal digest set ("sha256:..." each) that
	// short-circuits the remote-side enumeration step. Useful when the
	// caller already knows the peer's blob inventory.
	AssumeRemoteHas []string

	// KeepGoing makes per-image errors non-fatal: the run accumulates
	// failures and exits non-zero with a final failure count, rather
	// than short-circuiting on the first error.
	KeepGoing bool

	// Retries is per-blob upload retry count. 0 → DefaultRetries.
	Retries int
	// RetryMaxDelay caps exponential backoff. 0 → DefaultMaxDelay.
	RetryMaxDelay time.Duration
}

// PushSide bundles the local-side dependencies that the orchestrator
// uses. Both real (the [Skopeo] wrapper) and fakes (test
// implementations) plug in here.
type PushSide struct {
	Skopeo    SkopeoLike
	FS        FS
	BaseDir   string
	Transport string // canonical, e.g. "containers-storage"
	OCIPath   string // required when Transport == "oci"
}

// PushPeerSide bundles the peer-side dependencies plus enumeration
// inputs. RemoteHas overrides the lister-based enumeration when set.
type PushPeerSide struct {
	Skopeo    SkopeoLike
	FS        FS
	BaseDir   string
	Transport string
	OCIPath   string

	// Lister is required when Transport == containers-storage / docker-daemon
	// and AssumeHas is empty (we then enumerate via Skopeo+Lister).
	Lister listInterface

	// AssumeHas, if non-nil, replaces enumeration entirely.
	AssumeHas DigestSet
}

// SkopeoLike abstracts [*Skopeo] so tests can substitute a fake. The
// methods are the four we drive in push/pull orchestration.
type SkopeoLike interface {
	Version(ctx context.Context) (string, error)
	InspectRaw(ctx context.Context, transport, ref string) ([]byte, error)
	CopyToOCI(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error
	CopyFromOCI(ctx context.Context, ociDir, sharedBlobDir, dstTransport, dstRef string) error
}

// PushImageReport is the per-image summary line surfaced in the CLI
// output. Errors land in Err; on success Err is nil.
type PushImageReport struct {
	Ref        ImageRef
	Sent       int   // blobs actually transferred
	Reused     int   // blobs the peer already had (skipped)
	BytesSent  int64 // sum of expected sizes of transferred blobs
	DryRun     bool
	Err        error
}

// PushResult is the aggregate of per-image reports.
type PushResult struct {
	Reports     []PushImageReport
	FailedCount int
}

// Push orchestrates the push direction for every ref in args.Images.
// The function honors --dry-run (no mutation anywhere), --keep-going
// (continue on per-image error), and --assume-remote-has (skip
// enumeration of the peer).
func Push(ctx context.Context, args PushArgs, local PushSide, peer PushPeerSide) (PushResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePushArgs(args, local, peer); err != nil {
		return PushResult{}, err
	}

	jobs := args.Jobs
	if jobs <= 0 {
		jobs = 4
	}

	remoteHas, err := resolveRemoteHas(ctx, args.AssumeRemoteHas, peer)
	if err != nil {
		return PushResult{}, fmt.Errorf("push: enumerate remote: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "push.remote-has",
		slog.Int("blobs", len(remoteHas)),
		slog.Bool("from-flag", peer.AssumeHas != nil || len(args.AssumeRemoteHas) > 0),
	)

	var result PushResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := ParseImageRef(raw)
		if err != nil {
			rep := PushImageReport{Ref: ImageRef{Original: raw}, DryRun: args.DryRun, Err: err}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("push %q: %w", raw, err)
			}
			continue
		}

		rep := pushOne(ctx, args, local, peer, remoteHas, ref, jobs)
		result.Reports = append(result.Reports, rep)
		if rep.Err != nil {
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("push %s: %w", ref.String(), rep.Err)
			}
		}
	}
	return result, nil
}

// validatePushArgs returns an error for missing required-by-transport fields.
func validatePushArgs(args PushArgs, local PushSide, peer PushPeerSide) error {
	if len(args.Images) == 0 {
		return errors.New("push: no images")
	}
	if local.Transport == "" {
		return errors.New("push: local transport unset")
	}
	if peer.Transport == "" {
		return errors.New("push: remote transport unset")
	}
	if local.BaseDir == "" {
		return errors.New("push: local base dir unset")
	}
	if peer.BaseDir == "" {
		return errors.New("push: remote base dir unset")
	}
	return nil
}

// resolveRemoteHas builds the peer-has set, honoring the
// AssumeRemoteHas shortcut (per PLAN §4.2). The CLI flag values arrive
// as raw strings; PushPeerSide.AssumeHas is the higher-level form
// (already parsed to a DigestSet) used by tests.
func resolveRemoteHas(ctx context.Context, assumeRaw []string, peer PushPeerSide) (DigestSet, error) {
	if peer.AssumeHas != nil {
		return peer.AssumeHas, nil
	}
	if len(assumeRaw) > 0 {
		ds := NewDigestSet()
		for _, d := range assumeRaw {
			ds.Add(d)
		}
		return ds, nil
	}
	cfg := EnumerateConfig{
		Transport: peer.Transport,
		Skopeo:    peer.Skopeo,
		FS:        peer.FS,
		BaseDir:   peer.BaseDir,
	}
	switch peer.Transport {
	case TransportContainersStorage:
		if pl, ok := peer.Lister.(PodmanLister); ok {
			cfg.Podman = pl
		} else if peer.Lister != nil {
			cfg.Podman = listerAdapter{peer.Lister}
		}
	case TransportDockerDaemon:
		if dl, ok := peer.Lister.(DockerLister); ok {
			cfg.Docker = dl
		} else if peer.Lister != nil {
			cfg.Docker = listerAdapter{peer.Lister}
		}
	}
	return Enumerate(ctx, cfg)
}

// listerAdapter lets a generic listInterface satisfy both
// PodmanLister and DockerLister.
type listerAdapter struct{ inner listInterface }

func (l listerAdapter) ImageLs(ctx context.Context) ([]string, error) {
	return l.inner.ImageLs(ctx)
}

func pushOne(
	ctx context.Context,
	args PushArgs,
	local PushSide,
	peer PushPeerSide,
	remoteHas DigestSet,
	ref ImageRef,
	jobs int,
) PushImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PushImageReport{Ref: ref, DryRun: args.DryRun}

	store := NewStore(local.BaseDir)
	tagDirAbs, err := store.DumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	tagDirRel, err := RelDumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	localShareAbs := store.ShareDir()
	localShareRel := RelSharePath()
	remoteTagDirAbs := remoteDumpDirPosix(peer.BaseDir, ref)
	remoteTagDirRel := relRemoteDumpDir(ref)
	remoteShareAbs := PosixSharePath(peer.BaseDir)
	remoteShareRel := RelSharePath()

	closure, sizes, err := dumpAndDeriveClosure(ctx, args, local, ref, tagDirAbs, tagDirRel, localShareAbs, localShareRel)
	if err != nil {
		rep.Err = fmt.Errorf("dump: %w", err)
		return rep
	}

	pinned := NewDigestSet(closure.ManifestDigest, closure.ConfigDigest)
	toSend := Diff(closure.AllDigests(), remoteHas, pinned)

	for d := range closure.AllDigests() {
		if _, send := toSend[d]; !send {
			rep.Reused++
		}
	}

	if !args.DryRun {
		if err := transferTagDir(ctx, local.FS, tagDirRel, peer.FS, remoteTagDirRel); err != nil {
			rep.Err = fmt.Errorf("tag-dir sync: %w", err)
			return rep
		}
	}

	digestsSorted := sortedDigests(toSend)

	var bytesSent int64
	if args.DryRun {
		for _, d := range digestsSorted {
			bytesSent += sizes[d]
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "push.dry-run.plan",
			slog.String("ref", ref.String()),
			slog.Int("blobs", len(digestsSorted)),
			slog.Int64("bytes", bytesSent),
		)
		_, _ = localShareRel, remoteShareRel
	} else {
		runJobs := make([]Job, 0, len(digestsSorted))
		for _, d := range digestsSorted {
			expectedSize := sizes[d]
			srcPath, err := RelBlobPath(d)
			if err != nil {
				rep.Err = err
				return rep
			}
			dstPath := srcPath
			runJobs = append(runJobs, Job{
				ID: d,
				Run: func(ctx context.Context) error {
					return TransferBlob(ctx, local.FS, srcPath, peer.FS, dstPath, expectedSize)
				},
			})
		}
		res := RunPool(ctx, runJobs, jobs, RetryConfig{
			Retries:     args.Retries,
			MaxDelay:    args.RetryMaxDelay,
			IsRetryable: defaultIsRetryable,
		})
		if res.HasErrors() {
			rep.Err = res.JoinedError()
			return rep
		}
		for _, d := range digestsSorted {
			bytesSent += sizes[d]
		}
	}
	rep.Sent = len(digestsSorted)
	rep.BytesSent = bytesSent

	if !args.DryRun {
		if err := peer.Skopeo.CopyFromOCI(ctx, remoteTagDirAbs, remoteShareAbs, peer.Transport, ref.String()); err != nil {
			rep.Err = fmt.Errorf("remote load: %w", err)
			return rep
		}
	} else {
		logger.LogAttrs(ctx, slog.LevelInfo, "push.dry-run.would-load",
			slog.String("ref", ref.String()),
		)
	}
	return rep
}

// dumpAndDeriveClosure runs `skopeo copy ... oci:<tagDir>` (or, on
// --dry-run, `skopeo inspect --raw`) and returns the digest closure
// plus a digest→size map for the toSend ordering.
func dumpAndDeriveClosure(
	ctx context.Context,
	args PushArgs,
	local PushSide,
	ref ImageRef,
	tagDirAbs, tagDirRel, localShareAbs, localShareRel string,
) (Closure, map[string]int64, error) {
	srcTransport := local.Transport
	srcRef := ref.String()

	if !args.DryRun {
		if err := local.FS.MkdirAll(tagDirRel, 0o755); err != nil {
			return Closure{}, nil, fmt.Errorf("mkdir %s: %w", tagDirRel, err)
		}
		if err := local.Skopeo.CopyToOCI(ctx, srcTransport, srcRef, tagDirAbs, localShareAbs); err != nil {
			return Closure{}, nil, fmt.Errorf("skopeo copy: %w", err)
		}

		c, err := OCIClosure(fsBlobReader{fs: local.FS}, tagDirRel, localShareRel)
		if err != nil {
			return Closure{}, nil, fmt.Errorf("ociclosure: %w", err)
		}
		sizes := blobSizes(local.FS, c)
		return c, sizes, nil
	}

	// dry-run path: inspect manifest directly.
	raw, err := local.Skopeo.InspectRaw(ctx, srcTransport, srcRef)
	if err != nil {
		return Closure{}, nil, fmt.Errorf("skopeo inspect --raw: %w", err)
	}
	man, err := ParseManifest(raw)
	if err != nil {
		return Closure{}, nil, fmt.Errorf("parse manifest: %w", err)
	}
	c := Closure{
		ManifestDigest: DigestBytes(raw),
		ConfigDigest:   man.Config.Digest,
		LayerDigests:   man.LayerDigests(),
	}
	sizes := map[string]int64{c.ManifestDigest: int64(len(raw))}
	if man.Config.Size > 0 {
		sizes[c.ConfigDigest] = man.Config.Size
	}
	for _, l := range man.Layers {
		if l.Size > 0 {
			sizes[l.Digest] = l.Size
		}
	}
	return c, sizes, nil
}

// blobSizes returns size for every digest in c.AllDigests() found
// under the FS's share/ directory (relative path). Missing blobs are
// assigned size 0.
func blobSizes(fs FS, c Closure) map[string]int64 {
	out := make(map[string]int64)
	for d := range c.AllDigests() {
		p, err := RelBlobPath(d)
		if err != nil {
			continue
		}
		if size, ok, err := statSize(fs, p); err == nil && ok {
			out[d] = size
		}
	}
	return out
}

// transferTagDir ships oci-layout + index.json from srcDir to dstDir
// using [SafeWrite] (atomic tmp+rename).
func transferTagDir(ctx context.Context, srcFS FS, srcDir string, dstFS FS, dstDir string) error {
	return CopyTagDirSmallFiles(ctx, srcFS, srcDir, dstFS, dstDir,
		[]string{"oci-layout", "index.json"})
}

// remoteDumpDirPosix returns the peer-side tagDir / digestDir for ref
// in slash-form (absolute, suitable for the remote skopeo CLI).
func remoteDumpDirPosix(base string, r ImageRef) string {
	if r.IsDigested() {
		return PosixDigestPath(base, r.Host, r.Path, r.Digest)
	}
	return PosixTagPath(base, r.Host, r.Path, r.Tag)
}

// relRemoteDumpDir returns the peer-side dump dir relative to the
// peer base — suitable for FS calls on a [*sftpfs.SftpFs] rooted at
// peer base.
func relRemoteDumpDir(r ImageRef) string {
	if r.IsDigested() {
		return RelDigestPath(r.Host, r.Path, r.Digest)
	}
	return RelTagPath(r.Host, r.Path, r.Tag)
}

// sortedDigests returns ds in lexical order so transfer scheduling is
// deterministic (helps with test assertions and log readability).
func sortedDigests(ds DigestSet) []string {
	out := ds.Slice()
	sort.Strings(out)
	return out
}

// defaultIsRetryable: every error is retryable except [io.EOF] (which
// shouldn't be surfaced from TransferBlob anyway) and CommandError —
// non-zero exits from skopeo are program-logic failures, not network
// glitches, so don't burn retry budget on them.
func defaultIsRetryable(err error) bool {
	if errors.Is(err, io.EOF) {
		return false
	}
	var ce *cli.CommandError
	if errors.As(err, &ce) {
		return false
	}
	return true
}

// SummaryLine returns the human-readable per-image summary string.
func (r PushImageReport) SummaryLine() string {
	if r.Err != nil {
		return fmt.Sprintf("%s ERROR: %v", r.Ref.String(), r.Err)
	}
	prefix := ""
	if r.DryRun {
		prefix = "DRY-RUN would: "
	}
	return fmt.Sprintf("%s%s pushed (new: %d, reused: %d, bytes: %d)",
		prefix, r.Ref.String(), r.Sent, r.Reused, r.BytesSent)
}

// path joiner for code that doesn't import "path"
var _ = path.Join
