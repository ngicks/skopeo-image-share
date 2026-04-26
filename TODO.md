# skopeo-image-share — Implementation TODO

Derived from PLAN.md. Ordered top-down: earlier items unblock later ones.
Each item is sized to be a single PR-scale commit. Mark with `[x]` when done.

`docker-daemon:` is in scope for v1, on equal footing with
`containers-storage:` and `oci:`.

---

## Phase 0 — Scaffold

- [x] **0.1** Invoke `scaffold-go-cobra` skill with:
  project name `skopeo-image-share`, output dir `.`,
  module `github.com/ngicks/skopeo-image-share`, subcommands `push` `pull`.
  Skill output is adapted so that `commands/` lives under the entrypoint
  directory, not at module root:
  - `cmd/skopeo-image-share/main.go`
  - `cmd/skopeo-image-share/commands/root.go`
  - `cmd/skopeo-image-share/commands/push.go`
  - `cmd/skopeo-image-share/commands/pull.go`
  - `cmd/internal/cmdsignals/signals.go`
  - `cmd/internal/stdiopipe/stdiopipe.go`
  Imports in `main.go` and `commands/*.go` updated to
  `github.com/ngicks/skopeo-image-share/cmd/skopeo-image-share/commands`.
- [x] **0.2** Create empty `pkg/skopeoimageshare/` directory with a package
  doc comment in `pkg/skopeoimageshare/doc.go`.
- [x] **0.3** `go mod tidy`; commit baseline (build succeeds, `push`/`pull`
  print help).

## Phase 1 — Foundations (`pkg/skopeoimageshare/`)

- [x] **1.1** `imageref.go`: parse `[host[:port]/]<repo-path>[:tag|@digest]`
  into a struct; canonicalize Docker Hub refs to `library/<name>`;
  derive the on-disk tag dir:
  `<base>/<host>/<repo-path>/_tags/<tag>/` or `.../_digests/<hex>/`.
  **Reject** refs whose repo-path contains a `_tags` or `_digests`
  segment. Table-driven unit tests cover the PLAN §3 examples
  (`docker.io/library/nginx:latest`, `ghcr.io/a/b/c:d`,
  `ghcr.io/a/b/c/d:latest`, `registry.example.com:5000/...`, digest ref).
- [x] **1.2** `store.go`: `Store` type wrapping `<base>` with methods:
  `ShareDir()`, `TmpDir()`, `LogDir()`, `TagDir(ref) string`,
  `DigestDir(ref) string`, `EnsureLayout(ctx) error`. Resolves
  `$XDG_DATA_HOME`. Tests with `t.TempDir()` confirm paths + layout
  creation are idempotent. Includes coverage for digest-pinned refs
  resolving to `_digests/<hex>/`.
- [x] **1.3** `diff.go`: pure-function set math — given `need set[digest]`
  and `have set[digest]`, return `toSend set[digest]` with the rule
  "manifest + config are always included regardless of `have`". Unit
  tests cover edge cases (empty have, full overlap, meta-blob
  preservation).

## Phase 2 — External command wrappers

- [ ] **2.0** **Skopeo flag/version verify** (was 9.1). Before locking
  wrapper signatures in 2.1, confirm against the target `skopeo` version
  (PLAN §4.1 "verify" list):
  - `--shared-blob-dir` / `--src-shared-blob-dir` / `--dest-shared-blob-dir`
    spellings on `copy`, `inspect`, and `delete`.
  - `--preserve-digests` interaction with `oci:` + shared blob dir.
  - Whether skopeo writes a `blobs/sha256/` stub under `<tag>/` even with
    `--dest-shared-blob-dir`.
  Pin a minimum skopeo version in README + go.mod comment. Update PLAN
  §4.1 in place if any flag spelling differs from what is documented
  there.
- [x] **2.1** `skopeo.go`: typed wrapper over the `skopeo` CLI. Methods:
  - `Version(ctx) (string, error)`
  - `InspectRaw(ctx, transport, ref) (manifestJSON []byte, err error)`
  - `CopyToOCI(ctx, srcTransport, srcRef, ociDir, sharedBlobDir) error`
  - `CopyFromOCI(ctx, ociDir, sharedBlobDir, dstTransport, dstRef) error`
  All invocations pass `--preserve-digests`. stderr is streamed to slog
  at debug level; argv (with creds redacted) is logged at debug level
  per PLAN §8. Error returns include argv + exit code + tail of stderr
  (`%w`-wrapped). Unit tests use a fake `skopeo` binary on `$PATH` via
  `t.TempDir()` + shim script.
- [x] **2.2** `podman.go` and `docker.go`: typed wrappers over `podman`
  and `docker` CLIs. Both transports are v1. Methods:
  - `Version(ctx) (string, error)`
  - `ImageLs(ctx) (refs []string, err error)` via
    `podman image ls --format json` and `docker image ls --format json`
    respectively.
  Argv-redaction + stderr-to-slog + error-wrapping rules from 2.1 apply.
  Unit tests via shim scripts on `$PATH`.
- [x] **2.3** `manifest.go`: minimal OCI / Docker manifest JSON types
  needed to extract `config.digest` and `layers[].digest`. Cover both
  media types; a `ParseManifest([]byte) (Manifest, error)` helper
  selects based on `mediaType`. Table-driven unit tests on golden
  fixtures (one OCI, one Docker v2.2).
- [x] **2.4** `ociclosure.go`: OCI-layout digest closure walker. Given
  a tag dir + shared blob dir, read `<tag>/index.json` → resolve the
  manifest descriptor → load manifest blob from `<share>/<digest>` →
  return `{manifestDigest, configDigest, layerDigests}`. This is the
  algorithm referenced by PLAN §5.4.b and §7.2.b. Unit tests on a
  hand-built fixture covering: OCI manifest, Docker v2.2 manifest,
  missing manifest blob → typed error.

## Phase 3 — Remote (`pkg/skopeoimageshare/remote.go`)

- [x] **3.1** SSH dial: given either an ssh config name or separate
  host/user/port settings, build an `*ssh.Client`
  using `golang.org/x/crypto/ssh` + default keys (`~/.ssh/id_*`) +
  ssh-agent via `SSH_AUTH_SOCK`. `known_hosts` verification via
  `golang.org/x/crypto/ssh/knownhosts`. No `~/.ssh/config` parsing in
  v1 (stretch).
- [x] **3.2** `Remote` type wrapping `*ssh.Client` + `*sftp.Client`. Helpers:
  - `Run(ctx, argv ...string) (stdout, stderr []byte, err error)` for
    one-shot remote commands (`skopeo inspect`, `skopeo copy`,
    `podman image ls`, `printf XDG_DATA_HOME …`).
  - `ResolveBaseDir(ctx) (string, error)` via remote
    `printf %s "${XDG_DATA_HOME:-$HOME/.local/share}"`.
  - `Skopeo()`, `Podman()`, `Docker()` returning wrappers whose
    `exec.Cmd` equivalent runs via SSH session instead of local
    `exec.Command`.
- [x] **3.3** `ProbeSSH(ctx, target) error`: shell out to `ssh -G <host>`
  + a no-op `ssh <host> true` to surface ProxyCommand/Include/etc. errors
  via the user's normal ssh config codepath before we attempt the
  in-process dial (PLAN §4.3). Pure subprocess wrapper; no external
  state needed.
- [x] **3.4** Forced-close on context cancel (PLAN §8): goroutine that
  watches ctx, waits 2s grace, then calls `sftp.Close()` +
  `ssh.Close()`. Test with a fake local sshd (or skip if unavailable
  in CI — mark t.Skip).
- [x] **3.5** Blocked-write cancellation seam (PLAN §8). If 3.4 cannot be
  exercised in CI, add a unit-level test that drives the same
  forced-close path against a stub `io.WriteCloser` (no real sshd):
  start a write that blocks on a slow `Write`, cancel ctx, assert the
  write returns within the 2s grace.

## Phase 4 — Transfer engine (`pkg/skopeoimageshare/transfer.go`)

- [x] **4.1** Tag-dir sync using `fsutil.SafeWrite` (atomic tmp+rename)
  over an abstract `WriteFactory` interface. Two implementations: local
  (`os`), and SFTP-backed (`*sftp.Client`). Unit test: write file,
  interrupt mid-write, assert no partial final file remains.
- [x] **4.2** Shared-pool blob upload (`uploadBlob`): `.part` + resume
  via stat-size, skip when final exists with expected size, atomic
  rename on completion. Use
  `github.com/ngicks/go-fsys-helper/stream.Cancellable` to wrap the
  reader so every `Read` is cancellable. Unit test against an
  in-process `pkg/sftp` server on a temp dir; test resume by killing
  mid-write and re-running.
- [x] **4.3** Blob download mirror of 4.2 (pull direction). Same resume
  semantics. **Explicit test owner** for pull-direction resume:
  start download, kill mid-stream, re-run, assert `.part` size grew
  and final file digest matches filename (PLAN §10).
- [x] **4.4a** Worker pool + retry primitive: `workerPool(jobs []job, n int)`
  driving N goroutines off a job channel; per-job retry with
  exponential backoff (configurable `retries`, `retryMaxDelay`,
  defaults 5 tries / 30s) and reconnect callback invoked on classified
  network errors. Unit tests with a fake job that fails K times then
  succeeds.
- [x] **4.4b** `Push(ctx, args)` / `Pull(ctx, args)` orchestration
  entrypoints that compose `4.1` (tag-dir sync) + `4.2`/`4.3` (blob
  sync) on top of `4.4a`. Reconnect callback closes the `Remote` and
  redials. Symmetric: same engine, sides swapped.

## Phase 5 — Enumeration (`pkg/skopeoimageshare/enumerate.go`)

The output of every `Enumerate*` function is `peerHasBlobs: set[digest]`,
where the union covers per PLAN §4.2:

- `manifest` digests (the descriptor referenced from `index.json` /
  list response, **not** just layers/config)
- `config.digest`
- `layers[].digest`
- when applicable, the filename set of the peer's `<peer-base>/share/`

- [x] **5.1** `EnumerateContainersStorage(ctx, peer) (set[digest], error)`:
  `podman image ls --format json` → per-ref
  `skopeo inspect --raw containers-storage:<ref>` → union of
  `manifest` (digest of the inspected blob, computed by skopeo or
  derived from the raw bytes) + `config.digest` + `layers[].digest`.
  Unit tests with a fake `podman` + fake `skopeo` shim returning
  golden manifests; cover at least one OCI and one Docker v2.2 ref.
- [x] **5.2** `EnumerateDockerDaemon(ctx, peer) (set[digest], error)`:
  same shape via `docker image ls --format json` +
  `skopeo inspect --raw docker-daemon:<ref>`. Same union as 5.1.
  Unit tests via shim scripts on the same golden manifests.
- [x] **5.3** `EnumerateOCI(ctx, base) (set[digest], error)`: filesystem
  walk of our own `<base>/<host>/.../_tags/*/` and `.../_digests/*/`.
  For each tag/digest dir, run the 2.4 closure walker to derive
  `manifest+config+layers`. **Plus** include the filename set of
  `<base>/share/` directly (already-present blobs not necessarily
  reachable from any current tag are still inventory). Unit tests on
  hand-built fixtures with multiple tag dirs sharing layers, plus
  loose blobs in `share/`.
- [x] **5.4** Dispatcher `Enumerate(ctx, transport, ...) (set[digest], error)`
  that picks one of 5.1/5.2/5.3 based on the transport argument. Folded
  into whichever PR completes 5.1-5.3; no standalone commit.

## Phase 6 — `push` command

- [x] **6.1** Wire flags in `cmd/skopeo-image-share/commands/push.go` per
  PLAN §5 input list: `--image`, `--local-transport`, `--local-path`
  (only consumed for `oci:`), `--remote-host`, `--remote-transport`,
  `--remote-path` (only consumed for `oci:`), `--data-dir`, `--jobs`,
  `--dry-run`, `--assume-remote-has`, `--keep-going`, `--retries`,
  `--retry-max-delay`. Keep this file thin: validation only, then call
  into `pkg/skopeoimageshare`.
- [x] **6.2a** `runPush` step 1-3 (PLAN §5): validate flags + ensure
  layout, connect to remote (3.3 probe → 3.1 dial → 3.2 wrappers,
  version probe both ends, `ResolveBaseDir`), enumerate remote via
  `Enumerate(remoteTransport)`. **`--assume-remote-has`** short-circuits
  the enumeration step here: parse the digest list from flag values,
  use directly as `remoteHas`. Tests cover the assume-remote-has path
  (no enumeration calls made when flag is provided).
- [x] **6.2b** `runPush` step 4.a-b: per-image local
  `skopeo copy ... oci:<tagDir>` (or, when `--dry-run`,
  `skopeo inspect --raw ...`); derive digests via 2.4 closure walker
  (or via direct manifest parse on the dry-run path). Tests verify the
  dry-run path performs zero local writes (asserted by snapshotting
  the data dir before/after).
- [x] **6.2c** `runPush` step 4.c-d: compute `toSend` via 1.3 (always
  include manifest+config), invoke `transfer.Push(...)` (4.4b). Skipped
  on `--dry-run`. Tests verify `--dry-run` produces zero SFTP calls
  (use a counting fake `Remote`).
- [x] **6.2d** `runPush` step 4.e-5: remote `skopeo copy oci:<tagDir> ...`
  via SSH session; report summary `<ref> pushed (new: N, reused: M,
  bytes: B)` to stdout. `--dry-run` prefixes summary with
  `DRY-RUN would:` and skips the load step. `--keep-going`: per-image
  errors are accumulated, tool exits non-zero with a final failure
  count instead of short-circuiting (folded in here, not deferred to
  polish). Tests cover both dry-run summary formatting and
  `--keep-going` behavior with 2 images, one of which fails.
- [ ] **6.3** End-to-end smoke test: push a tiny image
  (`docker.io/library/busybox:latest` pulled once by test setup) over
  `ssh localhost`, assert remote `podman image ls` shows it.
  `testing.Short` skips this in CI without docker/ssh. Repeat for
  `docker-daemon:` source if `docker` is available; else mark skipped.

## Phase 7 — `pull` command

- [x] **7.1** Wire flags in `cmd/skopeo-image-share/commands/pull.go`,
  mirror of 6.1 (incl. `--local-path`, `--remote-path`,
  `--assume-local-has` as the pull-side equivalent of
  `--assume-remote-has`, `--retries`, `--retry-max-delay`).
- [x] **7.2a** `runPull` step 1 (PLAN §7): validate, connect, enumerate
  **local** via `Enumerate(localTransport)`. `--assume-local-has`
  short-circuits enumeration (mirror of 6.2a).
- [x] **7.2b** `runPull` step 2.a-b: remote `skopeo copy ... oci:<tagDir>`
  on the peer (or `skopeo inspect --raw` on `--dry-run`); derive digests
  via 2.4 closure walker over the **remote** layout (reads via SFTP).
  Tests cover dry-run zero-mutation on both ends.
- [x] **7.2c** `runPull` step 2.c-d: compute `toFetch`, invoke
  `transfer.Pull(...)`. Same dry-run guards as 6.2c.
- [x] **7.2d** `runPull` step 2.e-3: local `skopeo copy oci:<tagDir> ...`,
  summary, `--keep-going`. Mirror of 6.2d.
- [ ] **7.3** End-to-end smoke test (pull from localhost peer,
  same gating as 6.3, `docker-daemon:` variant included).

## Phase 8 — Polish

- [x] **8.1** slog configuration sweep: confirm argv of every
  skopeo/podman/docker invocation lands at debug level with redacted
  creds; info-level per-image and per-blob start/end events;
  error-level on non-zero exits + digest mismatches. (Most of this
  should already be in place from 2.1/2.2; this is a final consistency
  audit.)
- [x] **8.2** Error wrapping audit: every external-process failure
  includes argv + exit code + tail of stderr. Use `%w` for unwrap.
  Same — should mostly land per-task; this is a sweep.
- [x] **8.3** README update with a quick-start: install, ssh setup,
  one-image push example, one-image pull example, supported transports
  matrix.

## Phase 9 — Pre-release checks

- [x] **9.1** Verify `podman image ls --format json` and `docker image
  ls --format json` output shapes match the parsers in 2.2 against
  the targeted versions; pin minimums in README. Verified via
  `TestPodman_ImageLs_Fixture` against
  `testdata/podman-image-ls-json.json` and
  `TestDocker_ImageLs_Fixture` against
  `testdata/docker-image-ls-json.json`.
- [x] **9.2** Run `go vet`, `staticcheck`, `golangci-lint` if
  configured. Fix warnings.
- [ ] **9.3** Tag `v0.1.0`; draft release notes listing supported
  transports (containers-storage, docker-daemon, oci) and known
  limitations (no concurrency, no multi-arch, no auto-heal on digest
  mismatch).

---

## Tracking backlog (PLAN §9 — explicitly out of scope for v1)

Do **not** start these until v1 is shipped.

- `prune` subcommand
- `verify` subcommand
- Auto-heal on `skopeo copy` digest-mismatch failure
- Sign/verify via cosign
- Multi-arch `--all-platforms` fan-out
- Parallelism across images
- `~/.ssh/config` parsing / ProxyJump / ProxyCommand
- Split `pkg/skopeoimageshare` into sub-packages once API boundaries
  stabilize
