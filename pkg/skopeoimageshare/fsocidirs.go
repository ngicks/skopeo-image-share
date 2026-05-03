package skopeoimageshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"iter"
	"log/slog"
	"os"
	"path"
	"sync"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/stream"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/ocidir"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
	"golang.org/x/sync/errgroup"
)

// DefaultRemoteParallelism is the default upload concurrency for the
// SSH-backed [Remote] (matches `podman image pull --pull=missing`'s
// default of 3 simultaneous transfers).
const DefaultRemoteParallelism = 3

// FsOciDirs is an [OciDirs] backed by a [vroot.Fs] rooted at the
// [Store] base directory. Per-image dump dirs live under
// `<host>/<repo>/_tags/<tag>` (or `_digests/<hex>`); blobs live in
// the shared pool under `share/<algo>/<hex>`.
type FsOciDirs struct {
	fs vroot.Fs
	// worker pool concurrency limit
	limit int
}

// NewFsOciDirs returns an [FsOciDirs] over fs (rooted at the
// [Store] base). parallelism caps concurrent uploads in [PutBlobs];
// values ≤ 0 default to 1.
func NewFsOciDirs(fs vroot.Fs, limit int) *FsOciDirs {
	if limit <= 0 {
		limit = 1
	}
	return &FsOciDirs{fs: fs, limit: limit}
}

var _ OciDirs = (*FsOciDirs)(nil)

// Blob implements [OciDirs.Blob], reading from share/<algo>/<hex>.
func (d *FsOciDirs) Blob(ctx context.Context, dg digest.Digest, offset int64) (io.ReadCloser, int64, error) {
	_ = ctx
	algo, hex, err := ocidir.SplitDigest(string(dg))
	if err != nil {
		return nil, 0, err
	}
	return ocidir.OpenSeekedBlob(d.fs, path.Join(RelSharePath(), algo, hex), offset)
}

// Image implements [OciDirs.Image]: a tag-dir-scoped [ocidir.DirV1]
// view that reads index.json/oci-layout from ref's dump dir and blobs
// from the shared pool.
func (d *FsOciDirs) Image(ref imageref.ImageRef) ocidir.DirV1 {
	rel, err := RelDumpDir(ref)
	if err != nil {
		return errDirV1{err: fmt.Errorf("ocidir: image ref: %w", err)}
	}
	return sharedDir{fs: d.fs, dumpDir: rel, shareDir: RelSharePath()}
}

// PutBlobs implements [OciDirs.PutBlobs] with .part-resume + atomic
// rename. Blobs already complete at the destination are skipped
// (Open is not called); partial state is detected by the presence of
// `<dst>.part` and resumed by calling Open with the partial size as
// offset. Concurrency is capped by [FsOciDirs]'s parallelism. The
// first per-blob (or iterator) error cancels the group and short-
// circuits the remaining work via [errgroup.WithContext].
func (d *FsOciDirs) PutBlobs(ctx context.Context, blobs iter.Seq2[BlobUpload, error]) (PutBlobsResult, error) {
	var (
		result PutBlobsResult
		mu     sync.Mutex
	)
	g, gctx := errgroup.WithContext(ctx)
	g.SetLimit(d.limit)

	var iterErr error
	for bu, err := range blobs {
		if err != nil {
			iterErr = err
			break
		}
		if gctx.Err() != nil {
			break
		}
		g.Go(func() error {
			sent, err := d.putOne(gctx, bu)
			if err != nil {
				return fmt.Errorf("blob %s: %w", bu.Digest, err)
			}
			mu.Lock()
			defer mu.Unlock()
			if sent {
				result.Sent++
				result.BytesSent += bu.Size
			} else {
				result.Reused++
			}
			return nil
		})
	}
	return result, errors.Join(iterErr, g.Wait())
}

// putOne uploads one blob with .part-resume + atomic rename. Returns
// (true, nil) when bytes were copied; (false, nil) when the blob was
// already complete at the destination (Open was not called).
func (d *FsOciDirs) putOne(ctx context.Context, bu BlobUpload) (bool, error) {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	algo, hex, err := ocidir.SplitDigest(string(bu.Digest))
	if err != nil {
		return false, err
	}
	dstPath := path.Join(RelSharePath(), algo, hex)
	expectedSize := bu.Size

	if fi, err := d.fs.Stat(dstPath); err == nil {
		if fi.Size() == expectedSize {
			logger.LogAttrs(ctx, slog.LevelInfo, "putblobs.skip",
				slog.String("dst", dstPath),
				slog.Int64("size", fi.Size()),
			)
			return false, nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat dst %s: %w", dstPath, err)
	}

	if err := d.fs.MkdirAll(path.Dir(dstPath), 0o755); err != nil {
		return false, fmt.Errorf("mkdir %s: %w", path.Dir(dstPath), err)
	}

	part := dstPath + ".part"
	var startAt int64
	if fi, err := d.fs.Stat(part); err == nil {
		if fi.Size() > expectedSize {
			logger.LogAttrs(ctx, slog.LevelInfo, "putblobs.part-corrupt-restart",
				slog.String("part", part),
				slog.Int64("partSize", fi.Size()),
				slog.Int64("expected", expectedSize),
			)
			if err := d.fs.Remove(part); err != nil {
				return false, fmt.Errorf("remove oversize part: %w", err)
			}
			startAt = 0
		} else {
			startAt = fi.Size()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("stat part %s: %w", part, err)
	}

	if startAt == expectedSize {
		logger.LogAttrs(ctx, slog.LevelInfo, "putblobs.part-complete-rename",
			slog.String("part", part), slog.String("final", dstPath),
		)
		return true, d.fs.Rename(part, dstPath)
	}

	src, err := bu.Open(ctx, startAt)
	if err != nil {
		return false, fmt.Errorf("open src at %d: %w", startAt, err)
	}
	defer src.Close()

	dstF, err := d.fs.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return false, fmt.Errorf("open part %s: %w", part, err)
	}
	if startAt > 0 {
		if _, err := dstF.Seek(startAt, io.SeekStart); err != nil {
			dstF.Close()
			return false, fmt.Errorf("seek part to %d: %w", startAt, err)
		}
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "putblobs.copy",
		slog.String("dst", dstPath),
		slog.Int64("startAt", startAt),
		slog.Int64("expected", expectedSize),
	)

	r := stream.NewCancellable(ctx, src)
	buf := make([]byte, CopyBufferSize)
	if _, err := io.CopyBuffer(dstF, r, buf); err != nil {
		dstF.Close()
		return false, fmt.Errorf("copy: %w", err)
	}
	if err := dstF.Close(); err != nil {
		return false, fmt.Errorf("close part: %w", err)
	}
	if err := d.fs.Rename(part, dstPath); err != nil {
		return false, fmt.Errorf("rename %s -> %s: %w", part, dstPath, err)
	}
	logger.LogAttrs(ctx, slog.LevelInfo, "putblobs.done",
		slog.String("dst", dstPath),
		slog.Int64("size", expectedSize),
	)
	return true, nil
}

// PutTagFile implements [OciDirs.PutTagFile] using [fsutil.SafeWrite]
// (tmp + atomic rename). Used for index.json / oci-layout only.
func (d *FsOciDirs) PutTagFile(ctx context.Context, ref imageref.ImageRef, name string, data []byte) error {
	_ = ctx
	rel, err := RelDumpDir(ref)
	if err != nil {
		return err
	}
	if err := d.fs.MkdirAll(rel, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", rel, err)
	}
	return safeWriteOpt.Copy(
		d.fs,
		path.Join(rel, name),
		bytes.NewReader(data),
		os.ModePerm,
		nil,
		nil,
	)
}

// errDirV1 is a stub [ocidir.DirV1] returned by [FsOciDirs.Image]
// when the ref is malformed; every method surfaces err. This keeps
// Image's signature error-free per the [OciDirs] contract while still
// reporting the ref problem on first use.
type errDirV1 struct{ err error }

func (e errDirV1) Index() (v1.Index, error)             { return v1.Index{}, e.err }
func (e errDirV1) ImageLayout() (v1.ImageLayout, error) { return v1.ImageLayout{}, e.err }
func (e errDirV1) Blob(context.Context, digest.Digest, int64) (io.ReadCloser, int64, error) {
	return nil, 0, e.err
}
