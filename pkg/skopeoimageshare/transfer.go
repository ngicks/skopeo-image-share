package skopeoimageshare

import (
	"github.com/ngicks/go-fsys-helper/fsutil"
	"github.com/ngicks/go-fsys-helper/vroot"
)

// CopyBufferSize is the io.CopyBuffer chunk size used by [FsOciDirs]
// when streaming blob bytes. Tuned for SFTP throughput (the kernel
// limits SFTP payloads to ~32 KiB anyway, but a larger buffer reduces
// syscall overhead on the read side).
const CopyBufferSize = 256 * 1024

// safeWriteOpt is the [fsutil.SafeWriteOption] used by
// [FsOciDirs.PutTagFile] for tmp + atomic-rename writes of small
// per-image metadata files.
var safeWriteOpt = fsutil.SafeWriteOption[vroot.Fs, vroot.File]{
	TempFilePolicy: fsutil.NewTempFilePolicyDir[vroot.Fs]("__temp__"),
}
