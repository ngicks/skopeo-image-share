package skopeoimageshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

// recordingSkopeo is a fake [SkopeoLike] that records calls and lets
// tests fabricate responses.
type recordingSkopeo struct {
	versionRet  string
	inspectRaw  map[string][]byte
	copyToOCI   func(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error
	copyFromOCI func(ctx context.Context, ociDir, sharedBlobDir, dstTransport, dstRef string) error

	inspectCount  atomic.Int32
	copyToCount   atomic.Int32
	copyFromCount atomic.Int32
}

func (s *recordingSkopeo) Version(ctx context.Context) (string, error) {
	if s.versionRet == "" {
		return "fake-skopeo", nil
	}
	return s.versionRet, nil
}
func (s *recordingSkopeo) InspectRaw(ctx context.Context, transport, ref string) ([]byte, error) {
	s.inspectCount.Add(1)
	if data, ok := s.inspectRaw[transport+":"+ref]; ok {
		return data, nil
	}
	return nil, errors.New("no inspect fixture")
}
func (s *recordingSkopeo) CopyToOCI(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
	s.copyToCount.Add(1)
	if s.copyToOCI != nil {
		return s.copyToOCI(ctx, srcTransport, srcRef, ociDir, sharedBlobDir)
	}
	return nil
}
func (s *recordingSkopeo) CopyFromOCI(ctx context.Context, ociDir, sharedBlobDir, dstTransport, dstRef string) error {
	s.copyFromCount.Add(1)
	if s.copyFromOCI != nil {
		return s.copyFromOCI(ctx, ociDir, sharedBlobDir, dstTransport, dstRef)
	}
	return nil
}

// newSides constructs local + remote FSes (both osfs.NewUnrooted
// rooted at separate temp dirs) for orchestrator tests.
func newSides(t *testing.T) (localFS, remoteFS FS, localBase, remoteBase string) {
	t.Helper()
	localBase = t.TempDir()
	remoteBase = t.TempDir()
	var err error
	localFS, err = NewLocalFS(localBase)
	if err != nil {
		t.Fatal(err)
	}
	remoteFS, err = NewLocalFS(remoteBase)
	if err != nil {
		t.Fatal(err)
	}
	return
}

// seedDump materializes a fake oci: dump under tagDir + the manifest
// blob in shareDir/sha256.
func seedDump(t *testing.T, tagDir, shareDir string) (manifestDigest string) {
	t.Helper()
	must(t, os.MkdirAll(tagDir, 0o755))
	must(t, os.MkdirAll(filepath.Join(shareDir, "sha256"), 0o755))
	must(t, os.WriteFile(filepath.Join(tagDir, "oci-layout"), []byte(`{"imageLayoutVersion":"1.0.0"}`), 0o644))
	must(t, os.WriteFile(filepath.Join(tagDir, "index.json"), []byte(indexJSONFixture), 0o644))

	manHex := strings.Repeat("d", 64)
	manifestDigest = "sha256:" + manHex
	must(t, os.WriteFile(filepath.Join(shareDir, "sha256", manHex), []byte(ociManifestFixture), 0o644))
	must(t, os.WriteFile(filepath.Join(shareDir, "sha256", strings.Repeat("1", 64)), []byte("CFG"), 0o644))
	must(t, os.WriteFile(filepath.Join(shareDir, "sha256", strings.Repeat("a", 64)), []byte("L1"), 0o644))
	must(t, os.WriteFile(filepath.Join(shareDir, "sha256", strings.Repeat("b", 64)), []byte("L2"), 0o644))
	return manifestDigest
}

func TestPush_HappyPath_Sends_Then_Loads(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{
		copyToOCI: func(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
			return nil
		},
	}
	remoteSk := &recordingSkopeo{}

	res, err := Push(
		context.Background(),
		PushArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			Jobs:            2,
		},
		PushSide{Skopeo: localSk, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PushPeerSide{
			Skopeo:    remoteSk,
			FS:        remoteFS,
			BaseDir:   remoteBase,
			Transport: TransportContainersStorage,
			AssumeHas: NewDigestSet(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	if got := localSk.copyToCount.Load(); got != 1 {
		t.Errorf("local skopeo CopyToOCI called %d, want 1", got)
	}
	if got := remoteSk.copyFromCount.Load(); got != 1 {
		t.Errorf("remote skopeo CopyFromOCI called %d, want 1", got)
	}

	remoteShare := filepath.Join(remoteBase, "share", "sha256")
	for _, hex := range []string{strings.Repeat("d", 64), strings.Repeat("1", 64), strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if _, err := os.Stat(filepath.Join(remoteShare, hex)); err != nil {
			t.Errorf("expected remote blob %s present: %v", hex, err)
		}
	}
	for _, n := range []string{"index.json", "oci-layout"} {
		if _, err := os.Stat(filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1", n)); err != nil {
			t.Errorf("expected remote tag-dir file %s: %v", n, err)
		}
	}
}

func TestPush_ReusesRemoteHas(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	remoteHas := NewDigestSet(
		"sha256:"+strings.Repeat("a", 64),
		"sha256:"+strings.Repeat("b", 64),
	)

	res, err := Push(context.Background(),
		PushArgs{Images: []string{"ghcr.io/a/b:v1"}, LocalTransport: TransportContainersStorage, RemoteTransport: TransportContainersStorage},
		PushSide{Skopeo: &recordingSkopeo{}, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PushPeerSide{Skopeo: &recordingSkopeo{}, FS: remoteFS, BaseDir: remoteBase, Transport: TransportContainersStorage, AssumeHas: remoteHas},
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := res.Reports[0].Sent; got != 2 {
		t.Errorf("Sent = %d, want 2 (manifest + config; layers reused)", got)
	}
	if got := res.Reports[0].Reused; got != 2 {
		t.Errorf("Reused = %d, want 2 (the two layers)", got)
	}
}

func TestPush_DryRun_NoMutationsAnywhere(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(ociManifestFixture)
	localSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}
	remoteSk := &recordingSkopeo{}

	beforeLocal := snapshotDir(t, localBase)
	beforeRemote := snapshotDir(t, remoteBase)

	res, err := Push(context.Background(),
		PushArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			DryRun:          true,
		},
		PushSide{Skopeo: localSk, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PushPeerSide{
			Skopeo:    remoteSk,
			FS:        remoteFS,
			BaseDir:   remoteBase,
			Transport: TransportContainersStorage,
			AssumeHas: NewDigestSet(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run had failures: %+v", res.Reports)
	}
	if localSk.copyToCount.Load() != 0 {
		t.Errorf("dry-run called CopyToOCI %d times, want 0", localSk.copyToCount.Load())
	}
	if remoteSk.copyFromCount.Load() != 0 {
		t.Errorf("dry-run called CopyFromOCI %d times, want 0", remoteSk.copyFromCount.Load())
	}
	if got := localSk.inspectCount.Load(); got != 1 {
		t.Errorf("dry-run InspectRaw count %d, want 1", got)
	}
	afterLocal := snapshotDir(t, localBase)
	afterRemote := snapshotDir(t, remoteBase)
	if beforeLocal != afterLocal {
		t.Errorf("local mutated: before=%v after=%v", beforeLocal, afterLocal)
	}
	if beforeRemote != afterRemote {
		t.Errorf("remote mutated: before=%v after=%v", beforeRemote, afterRemote)
	}
	if !res.Reports[0].DryRun {
		t.Error("DryRun report flag not set")
	}
	if !strings.HasPrefix(res.Reports[0].SummaryLine(), "DRY-RUN would:") {
		t.Errorf("summary missing DRY-RUN prefix: %q", res.Reports[0].SummaryLine())
	}
}

func TestPush_KeepGoing_AccumulatesErrors(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "ok", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{
		copyToOCI: func(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
			if strings.Contains(srcRef, "fail") {
				return errors.New("simulated failure")
			}
			return nil
		},
	}

	res, err := Push(
		context.Background(),
		PushArgs{
			Images:          []string{"ghcr.io/a/ok:v1", "ghcr.io/a/fail:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			KeepGoing:       true,
		},
		PushSide{Skopeo: localSk, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PushPeerSide{Skopeo: &recordingSkopeo{}, FS: remoteFS, BaseDir: remoteBase, Transport: TransportContainersStorage, AssumeHas: NewDigestSet()},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 1 {
		t.Errorf("FailedCount = %d, want 1; reports=%+v", res.FailedCount, res.Reports)
	}
	if len(res.Reports) != 2 {
		t.Errorf("got %d reports, want 2", len(res.Reports))
	}
}

func TestPush_AssumeRemoteHasFromRawStrings_NoEnumeration(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(localBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(localBase, "share")
	seedDump(t, tagDir, shareDir)

	peerSk := &recordingSkopeo{} // InspectRaw fails by default

	_, err := Push(
		context.Background(),
		PushArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			AssumeRemoteHas: []string{"sha256:" + strings.Repeat("9", 64)},
		},
		PushSide{Skopeo: &recordingSkopeo{}, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PushPeerSide{
			Skopeo:    peerSk,
			FS:        remoteFS,
			BaseDir:   remoteBase,
			Transport: TransportContainersStorage,
			Lister:    &fakeLister{refs: nil},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if peerSk.inspectCount.Load() != 0 {
		t.Errorf("peer skopeo InspectRaw called %d times, want 0 (--assume-remote-has should skip enumeration)",
			peerSk.inspectCount.Load())
	}
}

// snapshotDir returns a stable string of all paths under root +
// their sizes, for "did anything change" assertions.
func snapshotDir(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	_ = filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, p)
		b.WriteString(rel)
		b.WriteByte('\t')
		if !fi.IsDir() {
			b.WriteString(int64ToStr(fi.Size()))
		} else {
			b.WriteString("dir")
		}
		b.WriteByte('\n')
		return nil
	})
	return b.String()
}

func int64ToStr(n int64) string {
	if n == 0 {
		return "0"
	}
	var digits [20]byte
	i := len(digits)
	for n > 0 {
		i--
		digits[i] = byte('0' + n%10)
		n /= 10
	}
	return string(digits[i:])
}
