package skopeoimageshare

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"os"
	"path"
	"sync"

	"github.com/ngicks/go-common/contextkey"
	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/stream"
	"github.com/ngicks/go-fsys-helper/vroot"
)

// CopyBufferSize is the io.CopyBuffer chunk size used when streaming
// blob bytes. Tuned for SFTP throughput (the kernel limits SFTP
// payloads to ~32 KiB anyway, but a larger buffer reduces syscall
// overhead on the read side).
const CopyBufferSize = 256 * 1024

// TransferBlob copies srcPath via srcFS to dstPath via dstFS with
// .part-based resume and atomic rename, taking the source size as the
// authoritative expectation.
//
// Resume rules:
//
//   - If dstPath already exists with size == expectedSize, return nil
//     immediately (skip).
//   - If dstPath+".part" exists with 0 < size <= expectedSize, the
//     upload resumes from that offset.
//   - If dstPath+".part" is larger than expectedSize (corrupt), it is
//     removed and the upload restarts from offset 0.
//
// Cancellation is per-Read via [stream.NewCancellable]; a blocked
// Write cannot be unblocked from this function — the caller must close
// the underlying SFTP client to abort a stuck Write.
func TransferBlob(ctx context.Context, srcFS vroot.Fs, srcPath string, dstFS vroot.Fs, dstPath string, expectedSize int64) error {
	logger := contextkey.ValueSlogLoggerDefault(ctx)
	logger.LogAttrs(ctx, slog.LevelDebug, "transfer.start",
		slog.String("src", srcPath),
		slog.String("dst", dstPath),
		slog.Int64("size", expectedSize),
	)

	if fi, err := dstFS.Stat(dstPath); err == nil {
		if fi.Size() == expectedSize {
			logger.LogAttrs(ctx, slog.LevelInfo, "transfer.skip",
				slog.String("dst", dstPath),
				slog.Int64("size", fi.Size()),
			)
			return nil
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("transfer: stat dst %s: %w", dstPath, err)
	}

	if err := dstFS.MkdirAll(path.Dir(dstPath), 0o755); err != nil {
		return fmt.Errorf("transfer: mkdir %s: %w", path.Dir(dstPath), err)
	}

	part := dstPath + ".part"
	var startAt int64
	if fi, err := dstFS.Stat(part); err == nil {
		if fi.Size() > expectedSize {
			logger.LogAttrs(
				ctx,
				slog.LevelInfo,
				"transfer.part-corrupt-restart",
				slog.String("part", part),
				slog.Int64("partSize", fi.Size()),
				slog.Int64("expected", expectedSize),
			)
			if err := dstFS.Remove(part); err != nil {
				return fmt.Errorf("transfer: remove oversize part: %w", err)
			}
			startAt = 0
		} else {
			startAt = fi.Size()
		}
	} else if !errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("transfer: stat part %s: %w", part, err)
	}

	if startAt == expectedSize {
		logger.LogAttrs(ctx, slog.LevelInfo, "transfer.part-complete-rename",
			slog.String("part", part), slog.String("final", dstPath),
		)
		return dstFS.Rename(part, dstPath)
	}

	srcF, err := srcFS.OpenFile(srcPath, os.O_RDONLY, 0)
	if err != nil {
		return fmt.Errorf("transfer: open src %s: %w", srcPath, err)
	}
	defer srcF.Close()
	if startAt > 0 {
		if _, err := srcF.Seek(startAt, io.SeekStart); err != nil {
			return fmt.Errorf("transfer: seek src to %d: %w", startAt, err)
		}
	}

	dstF, err := dstFS.OpenFile(part, os.O_WRONLY|os.O_CREATE, 0o644)
	if err != nil {
		return fmt.Errorf("transfer: open part %s: %w", part, err)
	}
	if startAt > 0 {
		if _, err := dstF.Seek(startAt, io.SeekStart); err != nil {
			dstF.Close()
			return fmt.Errorf("transfer: seek part to %d: %w", startAt, err)
		}
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "transfer.resume",
		slog.String("src", srcPath), slog.String("part", part),
		slog.Int64("startAt", startAt), slog.Int64("expected", expectedSize),
	)

	r := stream.NewCancellable(ctx, srcF)
	buf := make([]byte, CopyBufferSize)
	if _, err := io.CopyBuffer(dstF, r, buf); err != nil {
		dstF.Close()
		return fmt.Errorf("transfer: copy: %w", err)
	}
	if err := dstF.Close(); err != nil {
		return fmt.Errorf("transfer: close part: %w", err)
	}

	if err := dstFS.Rename(part, dstPath); err != nil {
		return fmt.Errorf("transfer: rename %s -> %s: %w", part, dstPath, err)
	}

	logger.LogAttrs(ctx, slog.LevelInfo, "transfer.done",
		slog.String("dst", dstPath),
		slog.Int64("size", expectedSize),
	)
	return nil
}

var safeWriteOpt = fsutil.SafeWriteOption[vroot.Fs, vroot.File]{
	TempFilePolicy: fsutil.NewTempFilePolicyDir[vroot.Fs]("__temp__"),
}

// CopyTagDirSmallFiles ships every regular file directly under srcDir to
// dstDir using [fsutil.SafeWriteOption]. It is intentionally non-recursive:
// skopeo's oci: layout writes only `oci-layout` and `index.json` directly
// under `<tag>/`, plus possibly an empty `blobs/sha256/` subdir which we
// don't need to ship (peer reads blobs from the shared pool).
//
// `entries` is the list of file basenames to ship (e.g. ["index.json",
// "oci-layout"]); pass exactly the files you want.
func CopyTagDirSmallFiles(ctx context.Context, srcFS vroot.Fs, srcDir string, dstFS vroot.Fs, dstDir string, entries []string) error {
	if err := dstFS.MkdirAll(dstDir, 0o755); err != nil {
		return fmt.Errorf("copytagdir: mkdir %s: %w", dstDir, err)
	}
	for _, name := range entries {
		data, err := vroot.ReadFile(srcFS, path.Join(srcDir, name))
		if err != nil {
			return fmt.Errorf("copytagdir: read %s: %w", name, err)
		}
		if err := safeWriteOpt.Copy(
			dstFS,
			path.Join(dstDir, name),
			bytes.NewReader(data),
			fs.ModePerm,
			nil,
			nil,
		); err != nil {
			return fmt.Errorf("copytagdir: safewrite %s: %w", name, err)
		}
	}
	return nil
}

// runTransfers runs each fn concurrently with at most parallelism in
// flight. ids[i] labels fns[i] in the joined error. The first per-fn
// failure is recorded and the function does not retry — callers that
// want retries should wrap fn themselves.
//
// Zero values: parallelism ≤ 0 → 1.
func runTransfers(
	ctx context.Context,
	parallelism int,
	ids []string,
	fns []func(context.Context) error,
) error {
	if parallelism <= 0 {
		parallelism = 1
	}

	sem := make(chan struct{}, parallelism)
	var (
		wg   sync.WaitGroup
		mu   sync.Mutex
		errs []error
	)
	for i, fn := range fns {
		select {
		case <-ctx.Done():
			wg.Wait()
			return errors.Join(append(errs, ctx.Err())...)
		case sem <- struct{}{}:
		}
		wg.Go(func() {
			defer func() { <-sem }()
			if err := fn(ctx); err != nil {
				mu.Lock()
				errs = append(errs, fmt.Errorf("%s: %w", ids[i], err))
				mu.Unlock()
			}
		})
	}
	wg.Wait()
	return errors.Join(errs...)
}
