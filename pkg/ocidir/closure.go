package ocidir

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// BlobReader reads blob contents addressed by digest from a shared
// blob dir. The default implementation hits the local filesystem
// ([LocalBlobReader]); other implementations (e.g. an SFTP-backed
// reader) plug in by satisfying this interface.
type BlobReader interface {
	// ReadBlob returns the contents of the blob with the given digest
	// from shareDir. shareDir is the base — implementations apply the
	// `<algorithm>/<hex>` layout themselves.
	ReadBlob(shareDir, digest string) ([]byte, error)
	// ReadIndexJSON returns the contents of dumpDir/index.json.
	ReadIndexJSON(dumpDir string) ([]byte, error)
}

// LocalBlobReader is a [BlobReader] backed by `os.ReadFile`. Suitable
// for the local side of push/pull and for tests.
type LocalBlobReader struct{}

// ReadBlob implements [BlobReader].
func (LocalBlobReader) ReadBlob(shareDir, digest string) ([]byte, error) {
	p, err := localBlobPath(shareDir, digest)
	if err != nil {
		return nil, err
	}
	return os.ReadFile(p)
}

// ReadIndexJSON implements [BlobReader].
func (LocalBlobReader) ReadIndexJSON(dumpDir string) ([]byte, error) {
	return os.ReadFile(filepath.Join(dumpDir, "index.json"))
}

// Closure is the digest closure of a single image dumped as oci:
// layout: the manifest descriptor (from index.json), and the config +
// layer descriptors from that manifest blob.
type Closure struct {
	ManifestDigest string
	ConfigDigest   string
	LayerDigests   []string
}

// AllDigests returns the full set: manifest, config, layers.
func (c Closure) AllDigests() map[string]struct{} {
	out := make(map[string]struct{}, 2+len(c.LayerDigests))
	out[c.ManifestDigest] = struct{}{}
	out[c.ConfigDigest] = struct{}{}
	for _, l := range c.LayerDigests {
		out[l] = struct{}{}
	}
	return out
}

// ErrMissingManifestBlob is returned by [ReadClosure] when the manifest
// blob referenced by index.json is not present in the shared blob dir.
var ErrMissingManifestBlob = errors.New("ocidir: manifest blob missing from shared blob dir")

// ReadClosure reads dumpDir/index.json, resolves the (single) manifest
// descriptor, loads the manifest blob from shareDir, and returns the
// digest closure.
func ReadClosure(reader BlobReader, dumpDir, shareDir string) (Closure, error) {
	idxData, err := reader.ReadIndexJSON(dumpDir)
	if err != nil {
		return Closure{}, fmt.Errorf("ocidir: read index.json: %w", err)
	}
	idx, err := ParseIndex(idxData)
	if err != nil {
		return Closure{}, fmt.Errorf("ocidir: %w", err)
	}
	mDesc := idx.Manifests[0]
	if mDesc.Digest == "" {
		return Closure{}, errors.New("ocidir: index.json manifest has no digest")
	}

	mData, err := reader.ReadBlob(shareDir, string(mDesc.Digest))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Closure{}, fmt.Errorf("%w: digest=%s", ErrMissingManifestBlob, mDesc.Digest)
		}
		return Closure{}, fmt.Errorf("ocidir: read manifest blob %s: %w", mDesc.Digest, err)
	}
	man, err := ParseManifest(mData)
	if err != nil {
		return Closure{}, fmt.Errorf("ocidir: parse manifest %s: %w", mDesc.Digest, err)
	}
	return Closure{
		ManifestDigest: string(mDesc.Digest),
		ConfigDigest:   string(man.Config.Digest),
		LayerDigests:   LayerDigests(man),
	}, nil
}

// localBlobPath joins shareDir with <algorithm>/<hex> for the blob.
func localBlobPath(shareDir, d string) (string, error) {
	algo, hex, err := SplitDigest(d)
	if err != nil {
		return "", err
	}
	return filepath.Join(shareDir, algo, hex), nil
}
