package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"time"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// PullArgs configures one [Local.Pull] invocation. Mirror of [PushArgs]
// for the pull direction; flag wiring lives in
// `cmd/skopeo-image-share/commands/pull.go`.
type PullArgs struct {
	Images []string

	Jobs int

	DryRun bool

	// AssumeLocalHas is the pull-direction equivalent of
	// [PushArgs.AssumeRemoteHas]: a literal digest set short-circuiting
	// local enumeration.
	AssumeLocalHas []string

	// AssumeLocalHasSet is the higher-level form of AssumeLocalHas
	// (already parsed to a [DigestSet]). When non-nil it takes
	// precedence over [PullArgs.AssumeLocalHas].
	AssumeLocalHasSet DigestSet

	KeepGoing bool

	Retries       int
	RetryMaxDelay time.Duration
}

// PullImageReport is the per-image summary line for pulls.
type PullImageReport struct {
	Ref          imageref.ImageRef
	Fetched      int   // blobs actually transferred
	Reused       int   // blobs already present locally
	BytesFetched int64 // sum of expected sizes of transferred blobs
	DryRun       bool
	Err          error
}

// PullResult is the aggregate of per-image pull reports.
type PullResult struct {
	Reports     []PullImageReport
	FailedCount int
}

// Pull orchestrates the pull direction (peer → local).
func (l *Local) Pull(ctx context.Context, args PullArgs, peer Remote) (PullResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePull(args, l, peer); err != nil {
		return PullResult{}, err
	}
	if err := l.Validate(ctx); err != nil {
		return PullResult{}, err
	}
	if err := peer.Validate(ctx); err != nil {
		return PullResult{}, err
	}

	jobs := args.Jobs
	if jobs <= 0 {
		jobs = 4
	}

	localHas, err := resolveLocalHas(ctx, args, l)
	if err != nil {
		return PullResult{}, fmt.Errorf("pull: enumerate local: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "pull.local-has",
		slog.Int("blobs", len(localHas)),
		slog.Bool("from-flag", args.AssumeLocalHasSet != nil || len(args.AssumeLocalHas) > 0),
	)

	var result PullResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := imageref.Parse(raw)
		if err != nil {
			rep := PullImageReport{Ref: imageref.ImageRef{Original: raw}, DryRun: args.DryRun, Err: err}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("pull %q: %w", raw, err)
			}
			continue
		}
		rep := pullOne(ctx, args, l, peer, localHas, ref, jobs)
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

func validatePull(args PullArgs, local *Local, peer Remote) error {
	if len(args.Images) == 0 {
		return errors.New("pull: no images")
	}
	if local.transport == "" {
		return errors.New("pull: local transport unset")
	}
	if peer.Transport() == "" {
		return errors.New("pull: remote transport unset")
	}
	if local.baseDir == "" {
		return errors.New("pull: local base dir unset")
	}
	if peer.BaseDir() == "" {
		return errors.New("pull: remote base dir unset")
	}
	return nil
}

func resolveLocalHas(ctx context.Context, args PullArgs, local *Local) (DigestSet, error) {
	if args.AssumeLocalHasSet != nil {
		return args.AssumeLocalHasSet, nil
	}
	if len(args.AssumeLocalHas) > 0 {
		ds := NewDigestSet()
		for _, d := range args.AssumeLocalHas {
			ds.Add(d)
		}
		return ds, nil
	}
	return local.List(ctx)
}

func pullOne(
	ctx context.Context,
	args PullArgs,
	local *Local,
	peer Remote,
	localHas DigestSet,
	ref imageref.ImageRef,
	jobs int,
) PullImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PullImageReport{Ref: ref, DryRun: args.DryRun}

	remoteStore := NewStore(peer.BaseDir())
	remoteTagDirNative, err := remoteStore.DumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	remoteTagDirAbs := filepath.ToSlash(remoteTagDirNative)
	remoteTagDirRel, err := RelDumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	remoteShareAbs := filepath.ToSlash(remoteStore.ShareDir())
	remoteShareRel := RelSharePath()
	localStore := NewStore(local.baseDir)
	localTagDirAbs, err := localStore.DumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	localTagDirRel := remoteTagDirRel
	localShareAbs := localStore.ShareDir()

	mDesc, man, err := dumpAndDeriveClosurePull(ctx, args, peer, ref, remoteTagDirAbs, remoteTagDirRel, remoteShareAbs, remoteShareRel)
	if err != nil {
		rep.Err = fmt.Errorf("remote dump: %w", err)
		return rep
	}

	descs := ocidir.AllDescriptors(mDesc, man)
	all := descriptorDigestSet(descs)
	sizes := descriptorSizes(descs)
	pinned := NewDigestSet(string(mDesc.Digest), string(man.Config.Digest))
	toFetch := Diff(all, localHas, pinned)

	for d := range all {
		if _, fetch := toFetch[d]; !fetch {
			rep.Reused++
		}
	}

	if !args.DryRun {
		if err := transferTagDir(ctx, peer.FS(), remoteTagDirRel, local.fs, localTagDirRel); err != nil {
			rep.Err = fmt.Errorf("tag-dir sync: %w", err)
			return rep
		}
	}

	digestsSorted := sortedDigests(toFetch)
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
					return TransferBlob(ctx, peer.FS(), relPath, local.fs, relPath, expectedSize)
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
		if err := local.skopeoCli.Copy(ctx,
			skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: localTagDirAbs, Arg2: ref.String()},
			skopeo.TransportRef{Transport: local.transport, Arg1: ref.String()},
			localShareAbs,
		); err != nil {
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
	peer Remote,
	ref imageref.ImageRef,
	tagDirAbs, tagDirRel, shareAbs, shareRel string,
) (v1.Descriptor, v1.Manifest, error) {
	src := skopeo.TransportRef{
		Transport: peer.Transport(),
		Arg1:      ref.String(),
	}

	if !args.DryRun {
		if err := peer.FS().MkdirAll(tagDirRel, 0o755); err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("mkdir %s: %w", tagDirRel, err)
		}
		if err := peer.Skopeo().Copy(ctx, src,
			skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
			shareAbs,
		); err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("skopeo copy: %w", err)
		}

		mDesc, man, err := ocidir.ReadManifest(sharedDir{
			fs:       peer.FS(),
			dumpDir:  tagDirRel,
			shareDir: shareRel,
		})
		if err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("ocidir: %w", err)
		}
		return mDesc, man, nil
	}

	raw, err := peer.Skopeo().InspectRaw(ctx, src)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("skopeo inspect --raw: %w", err)
	}
	man, err := ocidir.ParseManifest(raw)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("parse manifest: %w", err)
	}
	mDesc := v1.Descriptor{
		MediaType: man.MediaType,
		Digest:    digest.Digest(ocidir.DigestBytes(raw)),
		Size:      int64(len(raw)),
	}
	return mDesc, man, nil
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

