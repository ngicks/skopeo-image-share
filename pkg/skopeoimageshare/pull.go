package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"time"

	"github.com/ngicks/go-common/contextkey"
)

// PullArgs configures one [Pull] invocation. Mirror of [PushArgs] for
// the pull direction; flag wiring lives in
// `cmd/skopeo-image-share/commands/pull.go`.
type PullArgs struct {
	Images []string

	LocalTransport string
	LocalPath      string

	RemoteTransport string
	RemotePath      string

	DataDir string

	Jobs int

	DryRun bool

	// AssumeLocalHas is the pull-direction equivalent of
	// [PushArgs.AssumeRemoteHas]: a literal digest set short-circuiting
	// local enumeration.
	AssumeLocalHas []string

	KeepGoing bool

	Retries       int
	RetryMaxDelay time.Duration
}

// PullSide and PullPeerSide are the same shape as [PushSide] /
// [PushPeerSide] — separate types let the orchestrator address them
// distinctly when reading the code.
type PullSide struct {
	Skopeo    SkopeoLike
	FS        FS
	BaseDir   string
	Transport string
	OCIPath   string

	Lister    listInterface
	AssumeHas DigestSet
}

// PullPeerSide is the remote (source-of-truth) side for the pull
// direction.
type PullPeerSide struct {
	Skopeo    SkopeoLike
	FS        FS
	BaseDir   string
	Transport string
	OCIPath   string
}

// PullImageReport is the per-image summary line for pulls.
type PullImageReport struct {
	Ref         ImageRef
	Fetched     int   // blobs actually transferred
	Reused      int   // blobs already present locally
	BytesFetched int64
	DryRun      bool
	Err         error
}

// PullResult is the aggregate of per-image pull reports.
type PullResult struct {
	Reports     []PullImageReport
	FailedCount int
}

// Pull orchestrates the pull direction (peer → local). Behavior
// matches PLAN §7 with sides swapped from Push.
func Pull(ctx context.Context, args PullArgs, local PullSide, peer PullPeerSide) (PullResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePullArgs(args, local, peer); err != nil {
		return PullResult{}, err
	}

	jobs := args.Jobs
	if jobs <= 0 {
		jobs = 4
	}

	localHas, err := resolveLocalHas(ctx, args.AssumeLocalHas, local)
	if err != nil {
		return PullResult{}, fmt.Errorf("pull: enumerate local: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "pull.local-has",
		slog.Int("blobs", len(localHas)),
		slog.Bool("from-flag", local.AssumeHas != nil || len(args.AssumeLocalHas) > 0),
	)

	var result PullResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := ParseImageRef(raw)
		if err != nil {
			rep := PullImageReport{Ref: ImageRef{Original: raw}, DryRun: args.DryRun, Err: err}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("pull %q: %w", raw, err)
			}
			continue
		}
		rep := pullOne(ctx, args, local, peer, localHas, ref, jobs)
		result.Reports = append(result.Reports, rep)
		if rep.Err != nil {
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("pull %s: %w", ref.String(), rep.Err)
			}
		}
	}
	return result, nil
}

func validatePullArgs(args PullArgs, local PullSide, peer PullPeerSide) error {
	if len(args.Images) == 0 {
		return errors.New("pull: no images")
	}
	if local.Transport == "" {
		return errors.New("pull: local transport unset")
	}
	if peer.Transport == "" {
		return errors.New("pull: remote transport unset")
	}
	if local.BaseDir == "" {
		return errors.New("pull: local base dir unset")
	}
	if peer.BaseDir == "" {
		return errors.New("pull: remote base dir unset")
	}
	return nil
}

func resolveLocalHas(ctx context.Context, assumeRaw []string, local PullSide) (DigestSet, error) {
	if local.AssumeHas != nil {
		return local.AssumeHas, nil
	}
	if len(assumeRaw) > 0 {
		ds := NewDigestSet()
		for _, d := range assumeRaw {
			ds.Add(d)
		}
		return ds, nil
	}
	cfg := EnumerateConfig{
		Transport: local.Transport,
		Skopeo:    local.Skopeo,
		FS:        local.FS,
		BaseDir:   local.BaseDir,
	}
	switch local.Transport {
	case TransportContainersStorage:
		if local.Lister != nil {
			cfg.Podman = listerAdapter{local.Lister}
		}
	case TransportDockerDaemon:
		if local.Lister != nil {
			cfg.Docker = listerAdapter{local.Lister}
		}
	}
	return Enumerate(ctx, cfg)
}

func pullOne(
	ctx context.Context,
	args PullArgs,
	local PullSide,
	peer PullPeerSide,
	localHas DigestSet,
	ref ImageRef,
	jobs int,
) PullImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PullImageReport{Ref: ref, DryRun: args.DryRun}

	remoteTagDirAbs := remoteDumpDirPosix(peer.BaseDir, ref)
	remoteTagDirRel := relRemoteDumpDir(ref)
	remoteShareAbs := PosixSharePath(peer.BaseDir)
	remoteShareRel := RelSharePath()
	localStore := NewStore(local.BaseDir)
	localTagDirAbs, err := localStore.DumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	localTagDirRel, err := RelDumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	localShareAbs := localStore.ShareDir()
	_ = localShareAbs

	closure, sizes, err := dumpAndDeriveClosurePull(ctx, args, peer, ref, remoteTagDirAbs, remoteTagDirRel, remoteShareAbs, remoteShareRel)
	if err != nil {
		rep.Err = fmt.Errorf("remote dump: %w", err)
		return rep
	}

	pinned := NewDigestSet(closure.ManifestDigest, closure.ConfigDigest)
	toFetch := Diff(closure.AllDigests(), localHas, pinned)

	for d := range closure.AllDigests() {
		if _, fetch := toFetch[d]; !fetch {
			rep.Reused++
		}
	}

	if !args.DryRun {
		if err := transferTagDir(ctx, peer.FS, remoteTagDirRel, local.FS, localTagDirRel); err != nil {
			rep.Err = fmt.Errorf("tag-dir sync: %w", err)
			return rep
		}
	}

	digestsSorted := sortedDigestsPull(toFetch)
	var bytesFetched int64
	if args.DryRun {
		for _, d := range digestsSorted {
			bytesFetched += sizes[d]
		}
		logger.LogAttrs(ctx, slog.LevelInfo, "pull.dry-run.plan",
			slog.String("ref", ref.String()),
			slog.Int("blobs", len(digestsSorted)),
			slog.Int64("bytes", bytesFetched),
		)
	} else {
		runJobs := make([]Job, 0, len(digestsSorted))
		for _, d := range digestsSorted {
			expectedSize := sizes[d]
			relPath, err := RelBlobPath(d)
			if err != nil {
				rep.Err = err
				return rep
			}
			runJobs = append(runJobs, Job{
				ID: d,
				Run: func(ctx context.Context) error {
					return TransferBlob(ctx, peer.FS, relPath, local.FS, relPath, expectedSize)
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
			bytesFetched += sizes[d]
		}
	}
	rep.Fetched = len(digestsSorted)
	rep.BytesFetched = bytesFetched

	if !args.DryRun {
		if err := local.Skopeo.CopyFromOCI(ctx, localTagDirAbs, localShareAbs, local.Transport, ref.String()); err != nil {
			rep.Err = fmt.Errorf("local load: %w", err)
			return rep
		}
	} else {
		logger.LogAttrs(ctx, slog.LevelInfo, "pull.dry-run.would-load",
			slog.String("ref", ref.String()),
		)
	}
	return rep
}

func dumpAndDeriveClosurePull(
	ctx context.Context,
	args PullArgs,
	peer PullPeerSide,
	ref ImageRef,
	tagDirAbs, tagDirRel, shareAbs, shareRel string,
) (Closure, map[string]int64, error) {
	srcTransport := peer.Transport
	srcRef := ref.String()

	if !args.DryRun {
		if err := peer.FS.MkdirAll(tagDirRel, 0o755); err != nil {
			return Closure{}, nil, fmt.Errorf("mkdir %s: %w", tagDirRel, err)
		}
		if err := peer.Skopeo.CopyToOCI(ctx, srcTransport, srcRef, tagDirAbs, shareAbs); err != nil {
			return Closure{}, nil, fmt.Errorf("skopeo copy: %w", err)
		}

		c, err := OCIClosure(fsBlobReader{fs: peer.FS}, tagDirRel, shareRel)
		if err != nil {
			return Closure{}, nil, fmt.Errorf("ociclosure: %w", err)
		}
		sizes := blobSizes(peer.FS, c)
		return c, sizes, nil
	}

	raw, err := peer.Skopeo.InspectRaw(ctx, srcTransport, srcRef)
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

func sortedDigestsPull(ds DigestSet) []string {
	out := ds.Slice()
	sort.Strings(out)
	return out
}

// SummaryLine returns the human-readable per-image summary string.
func (r PullImageReport) SummaryLine() string {
	if r.Err != nil {
		return fmt.Sprintf("%s ERROR: %v", r.Ref.String(), r.Err)
	}
	prefix := ""
	if r.DryRun {
		prefix = "DRY-RUN would: "
	}
	return fmt.Sprintf("%s%s pulled (new: %d, reused: %d, bytes: %d)",
		prefix, r.Ref.String(), r.Fetched, r.Reused, r.BytesFetched)
}
