package skopeoimageshare

import (
	"context"
	"io"
	"iter"

	"github.com/ngicks/skopeo-image-share/pkg/imageref"
	"github.com/ngicks/skopeo-image-share/pkg/ocidir"
	"github.com/opencontainers/go-digest"
)

// OciDirs is a multi-image OCI store. It dispatches per-image reads
// via [OciDirs.Image] (returning an [ocidir.DirV1] view scoped to one
// image's tag dir) and exposes the shared blob pool — the union of
// every image's content-addressed storage — via [OciDirs.Blob].
//
// Write operations are bulk: [OciDirs.PutBlobs] streams an iterator
// of blobs through the store with implementation-chosen concurrency
// and resume; [OciDirs.PutTagFile] writes a single small per-image
// metadata file (`index.json` / `oci-layout`).
type OciDirs interface {
	// Blob reads from the shared blob pool. size is the total blob
	// size (not bytes remaining from offset); callers comparing
	// against a descriptor compare descriptor.Size against size, not
	// against bytes consumed from rc. Returns [os.ErrNotExist] when
	// the blob is missing.
	Blob(ctx context.Context, d digest.Digest, offset int64) (rc io.ReadCloser, size int64, err error)

	// Image returns an [ocidir.DirV1] view scoped to ref's tag dir
	// (its own index.json + oci-layout, blobs delegating to the
	// shared pool). Existence is not checked here; the first
	// Index() / Blob() call surfaces "not found".
	Image(ref imageref.ImageRef) ocidir.DirV1

	// PutBlobs uploads each blob the iterator yields. Implementations:
	//   - skip blobs already complete at the destination
	//     (digest + size match), counted against [PutBlobsResult.Reused]
	//   - resume from any partial state ('.part' or equivalent)
	//   - parallelize internally per the transport's policy
	// Iterator errors are joined into the returned error; per-blob
	// failures are likewise joined (the impl does not retry — wrap
	// the iterator if you want retry semantics). On error, blobs
	// already accepted remain on the destination.
	PutBlobs(ctx context.Context, blobs iter.Seq2[BlobUpload, error]) (PutBlobsResult, error)

	// PutTagFile writes a single small per-image metadata file
	// (e.g. "index.json" or "oci-layout") under ref's tag dir. Used
	// for the two well-known files only; manifest / config / layer
	// blobs go through [PutBlobs].
	PutTagFile(ctx context.Context, ref imageref.ImageRef, name string, data []byte) error
}

// BlobUpload describes one blob to ship through [OciDirs.PutBlobs].
//
// Open is a lazy factory: the implementation calls it only after
// deciding the blob actually needs (re)transfer (e.g., after a
// destination-side stat says the blob is missing or partial). It is
// called with the offset from which to start reading — 0 for fresh
// upload, a positive value to resume after partial state at the
// destination. The returned reader yields exactly Size-offset bytes
// when consumed to EOF; the implementation closes it.
//
// Open may be called multiple times across retries; each call returns
// a fresh reader positioned at the requested offset.
type BlobUpload struct {
	Digest digest.Digest
	Size   int64
	Open   func(ctx context.Context, offset int64) (io.ReadCloser, error)
}

// PutBlobsResult summarizes a [OciDirs.PutBlobs] run.
type PutBlobsResult struct {
	Sent      int   // blobs actually transferred (Open was called)
	Reused    int   // blobs already complete at destination (Open not called)
	BytesSent int64 // sum of Size for transferred blobs
}
