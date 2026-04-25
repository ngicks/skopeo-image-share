package skopeoimageshare

import (
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// BlobReader reads blob contents addressed by digest from a shared blob
// dir. The default implementation hits the local filesystem; the SFTP
// implementation reads via `*sftp.Client.ReadFile`.
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
func (c Closure) AllDigests() DigestSet {
	out := NewDigestSet(c.ManifestDigest, c.ConfigDigest)
	for _, l := range c.LayerDigests {
		out.Add(l)
	}
	return out
}

// ErrMissingManifestBlob is returned by [OCIClosure] when the manifest
// blob referenced by index.json is not present in the shared blob dir.
var ErrMissingManifestBlob = errors.New("ociclosure: manifest blob missing from shared blob dir")

// OCIClosure reads dumpDir/index.json, resolves the (single) manifest
// descriptor, loads the manifest blob from shareDir, and returns the
// digest closure.
func OCIClosure(reader BlobReader, dumpDir, shareDir string) (Closure, error) {
	idxData, err := reader.ReadIndexJSON(dumpDir)
	if err != nil {
		return Closure{}, fmt.Errorf("ociclosure: read index.json: %w", err)
	}
	idx, err := ParseIndex(idxData)
	if err != nil {
		return Closure{}, fmt.Errorf("ociclosure: %w", err)
	}
	mDesc := idx.Manifests[0]
	if mDesc.Digest == "" {
		return Closure{}, errors.New("ociclosure: index.json manifest has no digest")
	}

	mData, err := reader.ReadBlob(shareDir, mDesc.Digest)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Closure{}, fmt.Errorf("%w: digest=%s", ErrMissingManifestBlob, mDesc.Digest)
		}
		return Closure{}, fmt.Errorf("ociclosure: read manifest blob %s: %w", mDesc.Digest, err)
	}
	man, err := ParseManifest(mData)
	if err != nil {
		return Closure{}, fmt.Errorf("ociclosure: parse manifest %s: %w", mDesc.Digest, err)
	}
	return Closure{
		ManifestDigest: mDesc.Digest,
		ConfigDigest:   man.Config.Digest,
		LayerDigests:   man.LayerDigests(),
	}, nil
}

// SplitDigest splits "<algorithm>:<hex>" into its parts. It returns an
// error for malformed digests.
func SplitDigest(digest string) (algo, hex string, err error) {
	algo, hex, ok := strings.Cut(digest, ":")
	if !ok || algo == "" || hex == "" {
		return "", "", fmt.Errorf("malformed digest %q (want <algo>:<hex>)", digest)
	}
	return algo, hex, nil
}

// localBlobPath joins shareDir with <algorithm>/<hex> for the blob.
func localBlobPath(shareDir, digest string) (string, error) {
	algo, hex, err := SplitDigest(digest)
	if err != nil {
		return "", err
	}
	return filepath.Join(shareDir, algo, hex), nil
}

// PosixBlobPath is the slash-form analogue used for SFTP paths.
func PosixBlobPath(shareDir, digest string) (string, error) {
	algo, hex, err := SplitDigest(digest)
	if err != nil {
		return "", err
	}
	return path.Join(shareDir, algo, hex), nil
}
