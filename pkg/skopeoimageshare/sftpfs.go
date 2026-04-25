package skopeoimageshare

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"strings"
	"time"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/pkg/sftp"
)

// SFTPFS implements [vroot.Fs] (alias [FS]) over a *sftp.Client. It
// is rooted at Base — incoming paths are joined with Base before
// being passed to the SFTP client. Paths are POSIX (forward
// slashes).
type SFTPFS struct {
	Client *sftp.Client
	// Base is the absolute peer-side base directory; methods
	// internally prepend it to relative paths so the orchestrator can
	// pass relative paths consistently for both local and remote.
	Base string
}

// NewSFTPFS returns an [*SFTPFS] rooted at base (an absolute
// peer-side path).
func NewSFTPFS(c *sftp.Client, base string) *SFTPFS {
	return &SFTPFS{Client: c, Base: base}
}

// abs joins Base with name (slash form). "." resolves to Base.
func (s *SFTPFS) abs(name string) string {
	cleaned := path.Clean(name)
	if cleaned == "." || cleaned == "" {
		return s.Base
	}
	if strings.HasPrefix(cleaned, "/") {
		return cleaned
	}
	return path.Join(s.Base, cleaned)
}

// Chmod implements [vroot.Fs].
func (s *SFTPFS) Chmod(name string, mode fs.FileMode) error {
	return s.Client.Chmod(s.abs(name), mode)
}

// Chown implements [vroot.Fs].
func (s *SFTPFS) Chown(name string, uid, gid int) error {
	return s.Client.Chown(s.abs(name), uid, gid)
}

// Chtimes implements [vroot.Fs].
func (s *SFTPFS) Chtimes(name string, atime, mtime time.Time) error {
	return s.Client.Chtimes(s.abs(name), atime, mtime)
}

// Close implements [vroot.Fs]. It does not close the underlying
// *sftp.Client — that is the [Remote]'s job. Returning nil keeps
// fsutil's safe-write code path happy.
func (s *SFTPFS) Close() error { return nil }

// Create implements [vroot.Fs].
func (s *SFTPFS) Create(name string) (vroot.File, error) {
	return s.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
}

// Lchown implements [vroot.Fs] best-effort via Chown; sftp does not
// distinguish symlink-aware chown.
func (s *SFTPFS) Lchown(name string, uid, gid int) error {
	return s.Client.Chown(s.abs(name), uid, gid)
}

// Link implements [vroot.Fs] (hardlink, where supported).
func (s *SFTPFS) Link(oldname, newname string) error {
	return s.Client.Link(s.abs(oldname), s.abs(newname))
}

// Lstat implements [vroot.Fs].
func (s *SFTPFS) Lstat(name string) (fs.FileInfo, error) {
	return s.Client.Lstat(s.abs(name))
}

// Mkdir implements [vroot.Fs]. perm is best-effort applied via
// Chmod after the mkdir (sftp does not transmit perm directly).
func (s *SFTPFS) Mkdir(name string, perm fs.FileMode) error {
	if err := s.Client.Mkdir(s.abs(name)); err != nil {
		return err
	}
	if perm != 0 {
		_ = s.Client.Chmod(s.abs(name), perm)
	}
	return nil
}

// MkdirAll implements [vroot.Fs].
func (s *SFTPFS) MkdirAll(name string, perm fs.FileMode) error {
	if err := s.Client.MkdirAll(s.abs(name)); err != nil {
		return err
	}
	if perm != 0 {
		_ = s.Client.Chmod(s.abs(name), perm)
	}
	return nil
}

// Name implements [vroot.Fs].
func (s *SFTPFS) Name() string { return "sftp:" + s.Base }

// Open implements [vroot.Fs].
func (s *SFTPFS) Open(name string) (vroot.File, error) {
	return s.OpenFile(name, os.O_RDONLY, 0)
}

// OpenFile implements [vroot.Fs]. perm is best-effort applied via
// Chmod after the open succeeds.
func (s *SFTPFS) OpenFile(name string, flag int, perm fs.FileMode) (vroot.File, error) {
	abs := s.abs(name)
	f, err := s.Client.OpenFile(abs, flag)
	if err != nil {
		return nil, err
	}
	if perm != 0 && (flag&os.O_CREATE != 0) {
		_ = s.Client.Chmod(abs, perm)
	}
	return &sftpFile{File: f}, nil
}

// OpenRoot implements [vroot.Fs] but is not supported on SFTP.
func (s *SFTPFS) OpenRoot(name string) (vroot.Rooted, error) {
	return nil, vroot.ErrOpNotSupported
}

// ReadLink implements [vroot.Fs].
func (s *SFTPFS) ReadLink(name string) (string, error) {
	return s.Client.ReadLink(s.abs(name))
}

// Remove implements [vroot.Fs]. ENOENT is not an error.
func (s *SFTPFS) Remove(name string) error {
	if err := s.Client.Remove(s.abs(name)); err != nil && !isSFTPNotExist(err) {
		return err
	}
	return nil
}

// RemoveAll implements [vroot.Fs] via a recursive walk. The common
// case (Remove on a regular file) hits the fast path.
func (s *SFTPFS) RemoveAll(name string) error {
	abs := s.abs(name)
	if err := s.Client.Remove(abs); err == nil {
		return nil
	} else if isSFTPNotExist(err) {
		return nil
	}
	fi, err := s.Client.Lstat(abs)
	if err != nil {
		if isSFTPNotExist(err) {
			return nil
		}
		return err
	}
	if !fi.IsDir() {
		return s.Client.Remove(abs)
	}
	w := s.Client.Walk(abs)
	var paths []string
	for w.Step() {
		paths = append(paths, w.Path())
	}
	for i := len(paths) - 1; i >= 0; i-- {
		fi, err := s.Client.Lstat(paths[i])
		if err != nil {
			continue
		}
		if fi.IsDir() {
			_ = s.Client.RemoveDirectory(paths[i])
		} else {
			_ = s.Client.Remove(paths[i])
		}
	}
	return nil
}

// Rename implements [vroot.Fs] using POSIX rename when supported.
func (s *SFTPFS) Rename(oldname, newname string) error {
	if err := s.Client.PosixRename(s.abs(oldname), s.abs(newname)); err == nil {
		return nil
	} else if !errors.Is(err, sftp.ErrSSHFxOpUnsupported) {
		return err
	}
	return s.Client.Rename(s.abs(oldname), s.abs(newname))
}

// Stat implements [vroot.Fs].
func (s *SFTPFS) Stat(name string) (fs.FileInfo, error) {
	return s.Client.Stat(s.abs(name))
}

// Symlink implements [vroot.Fs].
func (s *SFTPFS) Symlink(oldname, newname string) error {
	return s.Client.Symlink(oldname, s.abs(newname))
}

// ReadDir implements the [vroot.ReadDirFs] optional optimization,
// adapting sftp's []os.FileInfo to []fs.DirEntry.
func (s *SFTPFS) ReadDir(name string) ([]fs.DirEntry, error) {
	fis, err := s.Client.ReadDir(s.abs(name))
	if err != nil {
		return nil, err
	}
	out := make([]fs.DirEntry, len(fis))
	for i, fi := range fis {
		out[i] = sftpDirEntry{fi: fi}
	}
	return out, nil
}

// sftpFile wraps *sftp.File to satisfy [vroot.File]. Methods that
// pkg/sftp does not expose return [vroot.ErrOpNotSupported].
type sftpFile struct{ *sftp.File }

func (f *sftpFile) Chown(uid, gid int) error    { return vroot.ErrOpNotSupported }
func (f *sftpFile) Fd() uintptr                 { return ^uintptr(0) }
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
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, fs.ErrNotExist) {
		return true
	}
	var stErr *sftp.StatusError
	if errors.As(err, &stErr) && stErr.FxCode() == sftp.ErrSSHFxNoSuchFile {
		return true
	}
	return false
}

// SFTPClientFromSSH dials SFTP over an *ssh.Client.
func SFTPClientFromSSH(c sshClient) (*sftp.Client, error) {
	sc, err := sftp.NewClient(c.Underlying())
	if err != nil {
		return nil, fmt.Errorf("sftp: %w", err)
	}
	return sc, nil
}
