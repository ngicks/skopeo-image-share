package skopeoimageshare

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-fsys-helper/vroot/osfs"
)

// FS is the filesystem abstraction the orchestrator drives. It is a
// strict alias of [vroot.Fs] — the local side is satisfied by
// [*osfs.Unrooted] (rooted at the application's base data dir, with
// relative paths) and the remote side by [*SFTPFS] (rooted at the
// peer's base, with relative paths internally translated to absolute
// SFTP paths).
type FS = vroot.Fs

// File is the per-file shape — alias of [vroot.File].
type File = vroot.File

// NewLocalFS returns a [FS] rooted at base (the application's data
// dir on this machine). All paths passed to its methods are relative
// to base.
func NewLocalFS(base string) (FS, error) {
	if err := os.MkdirAll(base, 0o755); err != nil {
		return nil, fmt.Errorf("local fs: mkdir base: %w", err)
	}
	u, err := osfs.NewUnrooted(base)
	if err != nil {
		return nil, fmt.Errorf("local fs: %w", err)
	}
	return u, nil
}

// SafeWrite atomically writes data to p (relative to f's root) via
// [fsutil.SafeWriteOption].
func SafeWrite(f FS, p string, data []byte) error {
	if err := f.MkdirAll(parentDir(p), 0o755); err != nil {
		return fmt.Errorf("safewrite: mkdir parent: %w", err)
	}
	switch f := f.(type) {
	case *osfs.Unrooted:
		opt := fsutil.SafeWriteOption[*osfs.Unrooted, vroot.File]{
			PostHooks:      []func(vroot.File, string) error{fsutil.SyncHook[vroot.File]},
			IgnoreCloseErr: true,
		}
		return opt.Copy(f, p, bytes.NewReader(data), 0o644, nil, nil)
	case *SFTPFS:
		opt := fsutil.SafeWriteOption[*SFTPFS, vroot.File]{}
		return opt.Copy(f, p, bytes.NewReader(data), 0o644, nil, nil)
	default:
		return fmt.Errorf("safewrite: unsupported FS %T", f)
	}
}

// readAllVia opens p for reading and returns its full contents. Used
// for small files (manifest blobs, index.json, oci-layout). Wraps
// vroot.ReadFile.
func readAllVia(f FS, p string) ([]byte, error) {
	return vroot.ReadFile(f, p)
}

// readDirVia is the FS-level ReadDir helper. It wraps vroot.ReadDir,
// which uses the optional ReadDirFs fast path when available
// (*SFTPFS exposes one) and falls back to OpenFile + File.ReadDir.
func readDirVia(f FS, p string) ([]fs.DirEntry, error) {
	return vroot.ReadDir(f, p)
}

// statSize is a small adapter that turns Stat's signature into
// "(size, exists, err)". Missing files are not treated as errors.
func statSize(f FS, p string) (int64, bool, error) {
	fi, err := f.Stat(p)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return fi.Size(), true, nil
}

// parentDir returns the slash-form parent directory of p. Different
// from [path.Dir] only in that "" is returned for top-level paths
// (vroot.Fs's MkdirAll on "" is well-defined).
func parentDir(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' {
			return p[:i]
		}
	}
	return "."
}
