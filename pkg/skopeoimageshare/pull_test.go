package skopeoimageshare

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPull_HappyPath(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{}
	peerSk := &recordingSkopeo{
		copyToOCI: func(ctx context.Context, srcTransport, srcRef, ociDir, sharedBlobDir string) error {
			return nil
		},
	}

	res, err := Pull(context.Background(),
		PullArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
		},
		PullSide{
			Skopeo:    localSk,
			FS:        localFS,
			BaseDir:   localBase,
			Transport: TransportContainersStorage,
			AssumeHas: NewDigestSet(),
		},
		PullPeerSide{Skopeo: peerSk, FS: remoteFS, BaseDir: remoteBase, Transport: TransportContainersStorage},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	if peerSk.copyToCount.Load() != 1 {
		t.Errorf("peer skopeo CopyToOCI called %d, want 1", peerSk.copyToCount.Load())
	}
	if localSk.copyFromCount.Load() != 1 {
		t.Errorf("local skopeo CopyFromOCI called %d, want 1", localSk.copyFromCount.Load())
	}
	for _, hex := range []string{strings.Repeat("d", 64), strings.Repeat("1", 64), strings.Repeat("a", 64), strings.Repeat("b", 64)} {
		if _, err := os.Stat(filepath.Join(localBase, "share", "sha256", hex)); err != nil {
			t.Errorf("expected local blob %s present: %v", hex, err)
		}
	}
}

func TestPull_DryRun_NoMutationsAnywhere(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	rawManifest := []byte(ociManifestFixture)
	peerSk := &recordingSkopeo{
		inspectRaw: map[string][]byte{
			"containers-storage:ghcr.io/a/b:v1": rawManifest,
		},
	}
	localSk := &recordingSkopeo{}

	beforeLocal := snapshotDir(t, localBase)
	beforeRemote := snapshotDir(t, remoteBase)

	res, err := Pull(context.Background(),
		PullArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			DryRun:          true,
		},
		PullSide{
			Skopeo:    localSk,
			FS:        localFS,
			BaseDir:   localBase,
			Transport: TransportContainersStorage,
			AssumeHas: NewDigestSet(),
		},
		PullPeerSide{Skopeo: peerSk, FS: remoteFS, BaseDir: remoteBase, Transport: TransportContainersStorage},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("dry-run had failures: %+v", res.Reports)
	}
	if peerSk.copyToCount.Load() != 0 {
		t.Errorf("dry-run called peer CopyToOCI %d, want 0", peerSk.copyToCount.Load())
	}
	if localSk.copyFromCount.Load() != 0 {
		t.Errorf("dry-run called local CopyFromOCI %d, want 0", localSk.copyFromCount.Load())
	}
	if afterLocal := snapshotDir(t, localBase); afterLocal != beforeLocal {
		t.Errorf("local mutated: before=%v after=%v", beforeLocal, afterLocal)
	}
	if afterRemote := snapshotDir(t, remoteBase); afterRemote != beforeRemote {
		t.Errorf("remote mutated: before=%v after=%v", beforeRemote, afterRemote)
	}
	if !res.Reports[0].DryRun {
		t.Error("DryRun report flag not set")
	}
	if !strings.HasPrefix(res.Reports[0].SummaryLine(), "DRY-RUN would:") {
		t.Errorf("summary missing DRY-RUN prefix: %q", res.Reports[0].SummaryLine())
	}
}

func TestPull_AssumeLocalHas_SkipsEnumeration(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)

	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	localSk := &recordingSkopeo{}
	_, err := Pull(context.Background(),
		PullArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
			AssumeLocalHas:  []string{"sha256:" + strings.Repeat("9", 64)},
		},
		PullSide{Skopeo: localSk, FS: localFS, BaseDir: localBase, Transport: TransportContainersStorage},
		PullPeerSide{Skopeo: &recordingSkopeo{copyToOCI: func(_ context.Context, _, _, _, _ string) error { return nil }}, FS: remoteFS, BaseDir: remoteBase, Transport: TransportContainersStorage},
	)
	if err != nil {
		t.Fatal(err)
	}
	if localSk.inspectCount.Load() != 0 {
		t.Errorf("local skopeo InspectRaw called %d, want 0", localSk.inspectCount.Load())
	}
}

func TestPull_ResumeFromInterruptedPart(t *testing.T) {
	t.Parallel()
	localFS, remoteFS, localBase, remoteBase := newSides(t)
	tagDir := filepath.Join(remoteBase, "ghcr.io", "a", "b", "_tags", "v1")
	shareDir := filepath.Join(remoteBase, "share")
	seedDump(t, tagDir, shareDir)

	localSha := filepath.Join(localBase, "share", "sha256")
	must(t, os.MkdirAll(localSha, 0o755))
	partPath := filepath.Join(localSha, strings.Repeat("a", 64)+".part")
	must(t, os.WriteFile(partPath, []byte("L"), 0o644))

	res, err := Pull(context.Background(),
		PullArgs{
			Images:          []string{"ghcr.io/a/b:v1"},
			LocalTransport:  TransportContainersStorage,
			RemoteTransport: TransportContainersStorage,
		},
		PullSide{
			Skopeo:    &recordingSkopeo{},
			FS:        localFS,
			BaseDir:   localBase,
			Transport: TransportContainersStorage,
			AssumeHas: NewDigestSet(),
		},
		PullPeerSide{
			Skopeo:    &recordingSkopeo{copyToOCI: func(ctx context.Context, _, _, _, _ string) error { return nil }},
			FS:        remoteFS,
			BaseDir:   remoteBase,
			Transport: TransportContainersStorage,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if res.FailedCount != 0 {
		t.Fatalf("FailedCount=%d, reports=%+v", res.FailedCount, res.Reports)
	}
	got, err := os.ReadFile(filepath.Join(localSha, strings.Repeat("a", 64)))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "L1" {
		t.Errorf("after resume got %q, want %q", got, "L1")
	}
	if _, err := os.Stat(partPath); !errors.Is(err, os.ErrNotExist) {
		t.Errorf(".part should be gone: stat err=%v", err)
	}
}
