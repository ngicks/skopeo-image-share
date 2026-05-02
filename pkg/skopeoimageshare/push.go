package skopeoimageshare

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sort"
	"time"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/skopeo-image-share/pkg/cli/skopeo"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// PushArgs configures one [Local.Push] invocation. Flags surfaced via
// the CLI (`cmd/skopeo-image-share/commands/push.go`) map 1:1 to fields
// on this struct; keep the cobra side a translation layer only.
type PushArgs struct {
	// Images is the list of refs to push (e.g. "ghcr.io/a/b:c").
	Images []string

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

	// AssumeRemoteHasSet is the higher-level form of AssumeRemoteHas
	// (already parsed to a digest set). When non-nil it takes
	// precedence over [PushArgs.AssumeRemoteHas].
	AssumeRemoteHasSet map[string]struct{}

	// KeepGoing makes per-image errors non-fatal: the run accumulates
	// failures and exits non-zero with a final failure count, rather
	// than short-circuiting on the first error.
	KeepGoing bool

	// Retries is per-blob upload retry count. 0 → DefaultRetries.
	Retries int
	// RetryMaxDelay caps exponential backoff. 0 → DefaultMaxDelay.
	RetryMaxDelay time.Duration
}

// SkopeoLike abstracts [*skopeo.Skopeo] so tests can substitute a fake.
// The methods are the three we drive in push/pull orchestration.
type SkopeoLike interface {
	Version(ctx context.Context) (string, error)
	Inspect(ctx context.Context, src skopeo.TransportRef, raw bool, sharedBlobDir string, extraArgs ...string) ([]byte, error)
	Copy(ctx context.Context, src, dst skopeo.TransportRef, sharedBlobDir string) error
}

// PushImageReport is the per-image summary line surfaced in the CLI
// output. Errors land in Err; on success Err is nil.
type PushImageReport struct {
	Ref       imageref.ImageRef
	Sent      int   // blobs actually transferred
	Reused    int   // blobs the peer already had (skipped)
	BytesSent int64 // sum of expected sizes of transferred blobs
	DryRun    bool
	Err       error
}

// PushResult is the aggregate of per-image reports.
type PushResult struct {
	Reports     []PushImageReport
	FailedCount int
}

// Push orchestrates the push direction (local → peer) for every ref in
// args.Images. Honors --dry-run (no mutation anywhere), --keep-going
// (continue on per-image error), and --assume-remote-has (skip
// enumeration of the peer).
func (l *Local) Push(ctx context.Context, args PushArgs, peer Remote) (PushResult, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)

	if err := validatePush(args, l, peer); err != nil {
		return PushResult{}, err
	}
	if err := l.Validate(ctx); err != nil {
		return PushResult{}, err
	}
	if err := peer.Validate(ctx); err != nil {
		return PushResult{}, err
	}

	jobs := args.Jobs
	if jobs <= 0 {
		jobs = 4
	}

	remoteHas, err := resolveRemoteHas(ctx, args, peer)
	if err != nil {
		return PushResult{}, fmt.Errorf("push: enumerate remote: %w", err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "push.remote-has",
		slog.Int("blobs", len(remoteHas)),
		slog.Bool("from-flag", args.AssumeRemoteHasSet != nil || len(args.AssumeRemoteHas) > 0),
	)

	var result PushResult
	for _, raw := range args.Images {
		if err := ctx.Err(); err != nil {
			return result, err
		}
		ref, err := imageref.Parse(raw)
		if err != nil {
			rep := PushImageReport{Ref: imageref.ImageRef{Original: raw}, DryRun: args.DryRun, Err: err}
			result.Reports = append(result.Reports, rep)
			result.FailedCount++
			if !args.KeepGoing {
				return result, fmt.Errorf("push %q: %w", raw, err)
			}
			continue
		}

		rep := pushOne(ctx, args, l, peer, remoteHas, ref, jobs)
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

// validatePush returns an error for missing required-by-transport fields.
func validatePush(args PushArgs, local *Local, peer Remote) error {
	if len(args.Images) == 0 {
		return errors.New("push: no images")
	}
	if local.transport == "" {
		return errors.New("push: local transport unset")
	}
	if peer.Transport() == "" {
		return errors.New("push: remote transport unset")
	}
	if local.baseDir == "" {
		return errors.New("push: local base dir unset")
	}
	if peer.BaseDir() == "" {
		return errors.New("push: remote base dir unset")
	}
	if peer.ReadOnly() {
		return errors.New("push: peer is read-only")
	}
	return nil
}

// resolveRemoteHas builds the peer-has set, honoring the assume-remote-has
// shortcut.
func resolveRemoteHas(ctx context.Context, args PushArgs, peer Remote) (map[string]struct{}, error) {
	if args.AssumeRemoteHasSet != nil {
		return args.AssumeRemoteHasSet, nil
	}
	if len(args.AssumeRemoteHas) > 0 {
		ds := make(map[string]struct{}, len(args.AssumeRemoteHas))
		for _, d := range args.AssumeRemoteHas {
			ds[d] = struct{}{}
		}
		return ds, nil
	}
	return peer.List(ctx)
}

func pushOne(
	ctx context.Context,
	args PushArgs,
	local *Local,
	peer Remote,
	remoteHas map[string]struct{},
	ref imageref.ImageRef,
	jobs int,
) PushImageReport {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	rep := PushImageReport{Ref: ref, DryRun: args.DryRun}

	store := NewStore(local.baseDir)
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
	remoteStore := NewStore(peer.BaseDir())
	remoteTagDirNative, err := remoteStore.DumpDir(ref)
	if err != nil {
		rep.Err = err
		return rep
	}
	remoteTagDirAbs := filepath.ToSlash(remoteTagDirNative)
	remoteTagDirRel := tagDirRel
	remoteShareAbs := filepath.ToSlash(remoteStore.ShareDir())

	mDesc, man, err := dumpAndDeriveClosurePush(ctx, args, local, ref, tagDirAbs, tagDirRel, localShareAbs, localShareRel)
	if err != nil {
		rep.Err = fmt.Errorf("dump: %w", err)
		return rep
	}

	descs := ocidir.AllDescriptors(mDesc, man)
	all := descriptorDigestSet(descs)
	sizes := descriptorSizes(descs)
	pinned := map[string]struct{}{
		string(mDesc.Digest):      {},
		string(man.Config.Digest): {},
	}
	toSend := mapKeyDiff(all, remoteHas, pinned)

	for d := range all {
		if _, send := toSend[d]; !send {
			rep.Reused++
		}
	}

	if !args.DryRun {
		if err := transferTagDir(ctx, local.fs, tagDirRel, peer.FS(), remoteTagDirRel); err != nil {
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
	} else {
		fns := make([]func(context.Context) error, 0, len(digestsSorted))
		for _, d := range digestsSorted {
			relPath, err := RelBlobPath(d)
			if err != nil {
				rep.Err = err
				return rep
			}
			expectedSize := sizes[d]
			fns = append(fns, func(ctx context.Context) error {
				return TransferBlob(ctx, local.fs, relPath, peer.FS(), relPath, expectedSize)
			})
		}
		if err := runTransfers(ctx, jobs, args.Retries, args.RetryMaxDelay, digestsSorted, fns); err != nil {
			rep.Err = err
			return rep
		}
		for _, d := range digestsSorted {
			bytesSent += sizes[d]
		}
	}
	rep.Sent = len(digestsSorted)
	rep.BytesSent = bytesSent

	if !args.DryRun {
		if err := peer.Skopeo().Copy(ctx,
			skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: remoteTagDirAbs, Arg2: ref.String()},
			skopeo.TransportRef{Transport: peer.Transport(), Arg1: ref.String()},
			remoteShareAbs,
		); err != nil {
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

// dumpAndDeriveClosurePush runs `skopeo copy ... oci:<tagDir>` (or, on
// --dry-run, `skopeo inspect --raw`) and returns the manifest
// descriptor + parsed manifest body. Use [ocidir.AllDescriptors] on
// the result to obtain the closure.
func dumpAndDeriveClosurePush(
	ctx context.Context,
	args PushArgs,
	local *Local,
	ref imageref.ImageRef,
	tagDirAbs, tagDirRel, localShareAbs, localShareRel string,
) (v1.Descriptor, v1.Manifest, error) {
	src := skopeo.TransportRef{
		Transport: local.transport,
		Arg1:      ref.String(),
	}

	if !args.DryRun {
		if err := local.fs.MkdirAll(tagDirRel, 0o755); err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("mkdir %s: %w", tagDirRel, err)
		}
		if err := local.skopeoCli.Copy(ctx, src,
			skopeo.TransportRef{Transport: skopeo.TransportOci, Arg1: tagDirAbs, Arg2: ref.String()},
			localShareAbs,
		); err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("skopeo copy: %w", err)
		}

		mDesc, man, err := ocidir.ReadManifest(sharedDir{
			fs:       local.fs,
			dumpDir:  tagDirRel,
			shareDir: localShareRel,
		})
		if err != nil {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("ocidir: %w", err)
		}
		return mDesc, man, nil
	}

	raw, err := local.skopeoCli.Inspect(ctx, src, true, "")
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

// descriptorDigestSet returns the digest set of every descriptor.
func descriptorDigestSet(descs []v1.Descriptor) map[string]struct{} {
	out := make(map[string]struct{}, len(descs))
	for _, d := range descs {
		out[string(d.Digest)] = struct{}{}
	}
	return out
}

// descriptorSizes returns the digest→size map for every descriptor
// with a non-zero Size. Descriptors with Size == 0 are omitted (size
// is not authoritative for them).
func descriptorSizes(descs []v1.Descriptor) map[string]int64 {
	out := make(map[string]int64, len(descs))
	for _, d := range descs {
		if d.Size > 0 {
			out[string(d.Digest)] = d.Size
		}
	}
	return out
}

// transferTagDir ships oci-layout + index.json from srcDir to dstDir
// using atomic tmp+rename.
func transferTagDir(ctx context.Context, srcFS vroot.Fs, srcDir string, dstFS vroot.Fs, dstDir string) error {
	return CopyTagDirSmallFiles(ctx, srcFS, srcDir, dstFS, dstDir,
		[]string{"oci-layout", "index.json"})
}

// sortedDigests returns ds in lexical order so transfer scheduling is
// deterministic (helps with test assertions and log readability).
func sortedDigests(ds map[string]struct{}) []string {
	out := make([]string, 0, len(ds))
	for d := range ds {
		out = append(out, d)
	}
	sort.Strings(out)
	return out
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
