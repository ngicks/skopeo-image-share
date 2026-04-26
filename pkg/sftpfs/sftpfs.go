// Package sftpfs implements [vroot.Unrooted] over a *sftp.Client.
package sftpfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"slices"
	"strings"
	"syscall"
	"time"

	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/ngicks/go-iterator-helper/hiter"
	"github.com/pkg/sftp"
)

var _ vroot.Unrooted = (*SftpFs)(nil)

// SftpFs implements [vroot.Unrooted] over a *sftp.Client. It is rooted
// at Base (an absolute peer-side path); incoming paths are cleaned,
// checked for traversal, then joined with Base before being passed to
// the SFTP client. Paths are POSIX (forward slashes).
//
// Like [osfs.Unrooted], path traversal via ".." or absolute paths is
// rejected with [vroot.ErrPathEscapes]. Symlink-based escapes are NOT
// prevented — that is the remote SFTP server's responsibility (e.g.
// sshd's ChrootDirectory or internal-sftp restrictions). Like the
// js/wasm *os.Root, this implementation is also vulnerable to TOCTOU.
type SftpFs struct {
	client *sftp.Client
	base   string
}

// New returns an [*SftpFs] rooted at base. base must be an absolute
// peer-side POSIX path; it is not validated against the remote.
func New(c *sftp.Client, base string) *SftpFs {
	return &SftpFs{client: c, base: base}
}

// Unrooted satisfies the [vroot.Unrooted] marker.
func (s *SftpFs) Unrooted() {}

// resolvePath cleans name, rejects path-traversal escapes, and joins
// with Base. Mirrors [osfs.Unrooted.resolvePath] but operates on POSIX
// paths.
func (s *SftpFs) resolvePath(name string) (string, error) {
	if s == nil || s.base == "" {
		panic("calling method of zero *SftpFs")
	}

	cleaned := path.Clean(name)
	if cleaned == "." {
		return s.base, nil
	}
	if !isLocalSlash(cleaned) {
		return "", vroot.ErrPathEscapes
	}
	return path.Join(s.base, cleaned), nil
}

// isLocalSlash reports whether p (already path.Clean'd) is a
// POSIX-style local path: not absolute, and does not escape its parent
// via "..".
func isLocalSlash(p string) bool {
	if p == "" {
		return false
	}
	if strings.HasPrefix(p, "/") {
		return false
	}
	if p == ".." || strings.HasPrefix(p, "../") {
		return false
	}
	return true
}

// Chmod implements [vroot.Fs].
func (s *SftpFs) Chmod(name string, mode fs.FileMode) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("chmod", name, err)
	}
	return s.client.Chmod(abs, mode)
}

// Chown implements [vroot.Fs].
func (s *SftpFs) Chown(name string, uid, gid int) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("chown", name, err)
	}
	return s.client.Chown(abs, uid, gid)
}

// Chtimes implements [vroot.Fs].
func (s *SftpFs) Chtimes(name string, atime, mtime time.Time) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("chtimes", name, err)
	}
	return s.client.Chtimes(abs, atime, mtime)
}

// Close implements [vroot.Fs]. It does not close the underlying
// *sftp.Client — that is the caller's job. Returning nil keeps
// fsutil's safe-write code path happy.
func (s *SftpFs) Close() error { return nil }

// Create implements [vroot.Fs].
func (s *SftpFs) Create(name string) (vroot.File, error) {
	return s.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Lchown implements [vroot.Fs] best-effort via Chown; sftp does not
// distinguish symlink-aware chown.
func (s *SftpFs) Lchown(name string, uid, gid int) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("lchown", name, err)
	}
	return s.client.Chown(abs, uid, gid)
}

// Link implements [vroot.Fs] (hardlink, where supported).
func (s *SftpFs) Link(oldname, newname string) error {
	oldAbs, err := s.resolvePath(oldname)
	if err != nil {
		return fsutil.WrapLinkErr("link", oldname, newname, err)
	}
	newAbs, err := s.resolvePath(newname)
	if err != nil {
		return fsutil.WrapLinkErr("link", oldname, newname, err)
	}
	return s.client.Link(oldAbs, newAbs)
}

// Lstat implements [vroot.Fs].
func (s *SftpFs) Lstat(name string) (fs.FileInfo, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return nil, fsutil.WrapPathErr("lstat", name, err)
	}
	if abs == s.base {
		return s.client.Stat(abs)
	}
	return s.client.Lstat(abs)
}

// Mkdir implements [vroot.Fs]. perm is best-effort applied via
// Chmod after the mkdir (sftp does not transmit perm directly).
func (s *SftpFs) Mkdir(name string, perm fs.FileMode) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("mkdir", name, err)
	}
	if err := s.client.Mkdir(abs); err != nil {
		return err
	}
	if perm != 0 {
		_ = s.client.Chmod(abs, perm)
	}
	return nil
}

// MkdirAll implements [vroot.Fs].
func (s *SftpFs) MkdirAll(name string, perm fs.FileMode) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("mkdir", name, err)
	}
	if err := s.client.MkdirAll(abs); err != nil {
		return err
	}
	if perm != 0 {
		_ = s.client.Chmod(abs, perm)
	}
	return nil
}

// Name implements [vroot.Fs].
func (s *SftpFs) Name() string { return "sftp:" + s.base }

// Open implements [vroot.Fs].
func (s *SftpFs) Open(name string) (vroot.File, error) {
	return s.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile implements [vroot.Fs]. perm is best-effort applied via
// Chmod after the open succeeds.
func (s *SftpFs) OpenFile(name string, flag int, perm fs.FileMode) (vroot.File, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return nil, fsutil.WrapPathErr("open", name, err)
	}
	f, err := s.client.OpenFile(abs, flag)
	if err != nil {
		return nil, err
	}
	if perm != 0 && (flag&os.O_CREATE != 0) {
		_ = s.client.Chmod(abs, perm)
	}
	return &sftpFile{File: f}, nil
}

// OpenRoot implements [vroot.Fs] but is not supported on SFTP — pkg/sftp
// has no openat-style primitive that would let us build a true rooted
// view.
func (s *SftpFs) OpenRoot(name string) (vroot.Rooted, error) {
	return nil, vroot.ErrOpNotSupported
}

// OpenUnrooted implements [vroot.Unrooted] by returning a new [*SftpFs]
// rooted at name (resolved against this Fs's base).
func (s *SftpFs) OpenUnrooted(name string) (vroot.Unrooted, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return nil, fsutil.WrapPathErr("open", name, err)
	}
	return &SftpFs{client: s.client, base: abs}, nil
}

// ReadLink implements [vroot.Fs].
func (s *SftpFs) ReadLink(name string) (string, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return "", fsutil.WrapPathErr("readlink", name, err)
	}
	if abs == s.base {
		return "", fsutil.WrapPathErr("readlink", abs, syscall.EINVAL)
	}
	return s.client.ReadLink(abs)
}

// Remove implements [vroot.Fs]. ENOENT is not an error.
func (s *SftpFs) Remove(name string) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("remove", name, err)
	}
	if err := s.client.Remove(abs); err != nil && !isSFTPNotExist(err) {
		return err
	}
	return nil
}

// RemoveAll implements [vroot.Fs] via a recursive walk. The common
// case (Remove on a regular file) hits the fast path.
func (s *SftpFs) RemoveAll(name string) error {
	abs, err := s.resolvePath(name)
	if err != nil {
		return fsutil.WrapPathErr("removeall", name, err)
	}
	if abs == s.base {
		return fsutil.WrapPathErr("removeall", ".", fs.ErrInvalid)
	}
	if err := s.client.Remove(abs); err == nil {
		return nil
	} else if isSFTPNotExist(err) {
		return nil
	}
	fi, err := s.client.Lstat(abs)
	if err != nil {
		if isSFTPNotExist(err) {
			return nil
		}
		return err
	}
	if !fi.IsDir() {
		return s.client.Remove(abs)
	}
	w := s.client.Walk(abs)
	var paths []string
	for w.Step() {
		paths = append(paths, w.Path())
	}
	for i := len(paths) - 1; i >= 0; i-- {
		fi, err := s.client.Lstat(paths[i])
		if err != nil {
			continue
		}
		if fi.IsDir() {
			_ = s.client.RemoveDirectory(paths[i])
		} else {
			_ = s.client.Remove(paths[i])
		}
	}
	return nil
}

// Rename implements [vroot.Fs] using POSIX rename when supported.
func (s *SftpFs) Rename(oldname, newname string) error {
	oldAbs, err := s.resolvePath(oldname)
	if err != nil {
		return fsutil.WrapLinkErr("rename", oldname, newname, err)
	}
	newAbs, err := s.resolvePath(newname)
	if err != nil {
		return fsutil.WrapLinkErr("rename", oldname, newname, err)
	}
	if err := s.client.PosixRename(oldAbs, newAbs); err == nil {
		return nil
	} else if !errors.Is(err, sftp.ErrSSHFxOpUnsupported) {
		return err
	}
	return s.client.Rename(oldAbs, newAbs)
}

// Stat implements [vroot.Fs].
func (s *SftpFs) Stat(name string) (fs.FileInfo, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return nil, fsutil.WrapPathErr("stat", name, err)
	}
	return s.client.Stat(abs)
}

// Symlink implements [vroot.Fs]. oldname (the link target) is stored
// verbatim — symlink targets may legitimately be relative or point
// outside the root, and containment is the remote's job.
func (s *SftpFs) Symlink(oldname, newname string) error {
	newAbs, err := s.resolvePath(newname)
	if err != nil {
		return fsutil.WrapLinkErr("symlink", oldname, newname, err)
	}
	return s.client.Symlink(oldname, newAbs)
}

// ReadDir implements the [vroot.ReadDirFs] optional optimization,
// adapting sftp's []os.FileInfo to []fs.DirEntry.
func (s *SftpFs) ReadDir(name string) ([]fs.DirEntry, error) {
	abs, err := s.resolvePath(name)
	if err != nil {
		return nil, fsutil.WrapPathErr("readdir", name, err)
	}
	fis, err := s.client.ReadDir(abs)
	if err != nil {
		return nil, err
	}
	return slices.Collect(
		hiter.Map(func(fi os.FileInfo) fs.DirEntry {
			return sftpDirEntry{fi: fi}
		},
			slices.Values(fis),
		),
	), nil
}

// Client returns the underlying *sftp.Client (for advanced use).
func (s *SftpFs) Client() *sftp.Client { return s.client }

// Base returns the absolute peer-side base directory.
func (s *SftpFs) Base() string { return s.base }

// sftpFile wraps *sftp.File to satisfy [vroot.File]. Methods that
// pkg/sftp does not expose return [vroot.ErrOpNotSupported].
type sftpFile struct{ *sftp.File }

func (f *sftpFile) Chown(uid, gid int) error { return vroot.ErrOpNotSupported }
func (f *sftpFile) Fd() uintptr              { return ^uintptr(0) }
func (f *sftpFile) ReadDir(n int) ([]fs.DirEntry, error) {
	return nil, vroot.ErrOpNotSupported
}

func (f *sftpFile) Readdir(n int) ([]fs.FileInfo, error) {
	return nil, vroot.ErrOpNotSupported
}

func (f *sftpFile) Readdirnames(n int) ([]string, error) {
	return nil, vroot.ErrOpNotSupported
}

func (f *sftpFile) WriteString(s string) (int, error) {
	return f.Write([]byte(s))
}

// sftpDirEntry adapts os.FileInfo to fs.DirEntry.
type sftpDirEntry struct{ fi os.FileInfo }

func (e sftpDirEntry) Name() string               { return e.fi.Name() }
func (e sftpDirEntry) IsDir() bool                { return e.fi.IsDir() }
func (e sftpDirEntry) Type() fs.FileMode          { return e.fi.Mode().Type() }
func (e sftpDirEntry) Info() (fs.FileInfo, error) { return e.fi, nil }

// isSFTPNotExist returns true if err represents a missing-file
// status, regardless of whether it was wrapped via fs.PathError or
// surfaced as a sftp.StatusError.
func isSFTPNotExist(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fs.ErrNotExist) {
		return true
	}
	stErr, ok := errors.AsType[*sftp.StatusError](err)
	if ok && stErr.FxCode() == sftp.ErrSSHFxNoSuchFile {
		return true
	}
	return false
}
