package ocidir

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"

	"github.com/ngicks/go-fsys-helper/vroot"
	"github.com/opencontainers/go-digest"
	v1 "github.com/opencontainers/image-spec/specs-go/v1"
)

// DirV1 is an OCI image-layout v1 directory. The methods expose the
// canonical files (`index.json`, `oci-layout`) plus blob lookup.
//
// More methods will be added as the OCI image-spec gains them
// (predeclared blobs, referrers, etc.). Implementations should accept
// missing optional files but return the standard `os.ErrNotExist` for
// any blob that is not present.
type DirV1 interface {
	// Index parses `index.json` and returns the typed [v1.Index].
	Index() (v1.Index, error)
	// ImageLayout parses `oci-layout` and returns the typed [v1.ImageLayout].
	ImageLayout() (v1.ImageLayout, error)
	// Blob reads the blob with the given digest from the dir's blob
	// pool. Returns [os.ErrNotExist] when missing.
	Blob(d digest.Digest) ([]byte, error)
}

// FsDir is a [DirV1] backed by a [vroot.Fs] rooted at an OCI dir.
// Blobs are read from the spec-default `blobs/<algo>/<hex>` location.
// Use a custom [DirV1] implementation when blobs live elsewhere
// (e.g. skopeo's --shared-blob-dir layout).
type FsDir struct {
	fs vroot.Fs
}

// NewFsDir returns an [FsDir] reading from fs (rooted at an OCI dir).
func NewFsDir(fs vroot.Fs) FsDir {
	return FsDir{fs: fs}
}

// Index implements [DirV1].
func (d FsDir) Index() (v1.Index, error) {
	data, err := vroot.ReadFile(d.fs, "index.json")
	if err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: read index.json: %w", err)
	}
	var idx v1.Index
	if err := json.Unmarshal(data, &idx); err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: parse index.json: %w", err)
	}
	if err := ValidateIndex(idx); err != nil {
		return v1.Index{}, fmt.Errorf("ocidir: %w", err)
	}
	return idx, nil
}

// ImageLayout implements [DirV1].
func (d FsDir) ImageLayout() (v1.ImageLayout, error) {
	data, err := vroot.ReadFile(d.fs, v1.ImageLayoutFile)
	if err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: read %s: %w", v1.ImageLayoutFile, err)
	}
	var l v1.ImageLayout
	if err := json.Unmarshal(data, &l); err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: parse %s: %w", v1.ImageLayoutFile, err)
	}
	if err := ValidateImageLayout(l); err != nil {
		return v1.ImageLayout{}, fmt.Errorf("ocidir: %w", err)
	}
	return l, nil
}

// Blob implements [DirV1].
func (d FsDir) Blob(dg digest.Digest) ([]byte, error) {
	algo, hex, err := SplitDigest(string(dg))
	if err != nil {
		return nil, err
	}
	data, err := vroot.ReadFile(d.fs, path.Join("blobs", algo, hex))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

// SharedFsDir pairs a [DirV1] (typically an [FsDir] rooted at the
// dump dir, providing Index + ImageLayout) with a separate
// [vroot.Fs] rooted at the shared blob pool. It models skopeo's
// `--shared-blob-dir` layout, where index.json + oci-layout live in
// one place and the per-image blobs live elsewhere.
//
// Blob reads `<blobs>/<algo>/<hex>`; Index and ImageLayout delegate
// to the dir field.
type SharedFsDir struct {
	dir   DirV1
	blobs vroot.Fs
}

// NewSharedFsDir returns a [SharedFsDir] that delegates Index and
// ImageLayout to dir and reads blobs from blobs (rooted at the share
// pool, layout `<algo>/<hex>`).
func NewSharedFsDir(dir DirV1, blobs vroot.Fs) SharedFsDir {
	return SharedFsDir{dir: dir, blobs: blobs}
}

// Index implements [DirV1].
func (d SharedFsDir) Index() (v1.Index, error) { return d.dir.Index() }

// ImageLayout implements [DirV1].
func (d SharedFsDir) ImageLayout() (v1.ImageLayout, error) { return d.dir.ImageLayout() }

// Blob implements [DirV1] reading from the dedicated blob FS.
func (d SharedFsDir) Blob(dg digest.Digest) ([]byte, error) {
	algo, hex, err := SplitDigest(string(dg))
	if err != nil {
		return nil, err
	}
	data, err := vroot.ReadFile(d.blobs, path.Join(algo, hex))
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) || errors.Is(err, os.ErrNotExist) {
			return nil, os.ErrNotExist
		}
		return nil, err
	}
	return data, nil
}

// ErrMissingManifestBlob is returned by [ReadManifest] when the
// manifest blob referenced by index.json is not present in the dir's
// blob pool.
var ErrMissingManifestBlob = errors.New("ocidir: manifest blob missing from blob pool")

// ReadManifest reads index.json from dir, resolves the (single) manifest
// descriptor, loads the manifest blob from the dir's blob pool, parses
// it, and returns the descriptor (size + digest + mediaType from the
// index) plus the parsed manifest body.
func ReadManifest(dir DirV1) (v1.Descriptor, v1.Manifest, error) {
	idx, err := dir.Index()
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, err
	}
	mDesc := idx.Manifests[0]
	if mDesc.Digest == "" {
		return v1.Descriptor{}, v1.Manifest{}, errors.New("ocidir: index.json manifest has no digest")
	}

	mData, err := dir.Blob(mDesc.Digest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("%w: digest=%s", ErrMissingManifestBlob, mDesc.Digest)
		}
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("ocidir: read manifest blob %s: %w", mDesc.Digest, err)
	}
	man, err := ParseManifest(mData)
	if err != nil {
		return v1.Descriptor{}, v1.Manifest{}, fmt.Errorf("ocidir: parse manifest %s: %w", mDesc.Digest, err)
	}
	return mDesc, man, nil
}

// AllDescriptors returns mDesc + m.Config + m.Layers... — every
// descriptor reachable from a single image manifest. Use this when
// you need the digest set or size map of the closure.
func AllDescriptors(mDesc v1.Descriptor, m v1.Manifest) []v1.Descriptor {
	out := make([]v1.Descriptor, 0, 2+len(m.Layers))
	out = append(out, mDesc, m.Config)
	out = append(out, m.Layers...)
	return out
}
