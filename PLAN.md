# skopeo-image-share — Plan

Status: draft #1 (for review)
Owner: @ngicks
Goal: share OCI images between two hosts efficiently (layer-level diff) over SSH,
without a registry, by driving `skopeo` + `podman` and using `pkg/sftp` for blob
transfer with resume.

---

## 1. Scope

- Two hosts: **local** (where the CLI runs) and **remote** (reachable by `ssh`).
- Two directions, as two subcommands of a single binary (`skopeo-image-share`):
  - `push` — `local -> remote`
  - `pull` — `remote -> local`
- Work at the **image blob level**, i.e. skip blob files whose digest the peer
  already has. No container-registry is involved on either end.
- Keep CLI "dumb" & idempotent: each invocation enumerates state on both sides
  from scratch (no persistent index), so interrupted runs just re-run.
- Out of scope for this milestone:
  - signing / cosign verification
  - multi-arch manifest list planning. In practice refs we see from
    `containers-storage:` / `docker-daemon:` are already single-platform
    (platform is resolved at pull time), so we do not handle manifest
    lists ourselves. Full `--all-platforms` fan-out from an `oci:` index
    source is out of scope.
  - running as a long-lived daemon
  - parallelism across _images_ (inside one image we may parallelize blobs)
  - **concurrent invocations** against the same `<base>/` or
    `<peer-base>/`. The tool is expected to run as a single per-user
    command; no locking on `share/`, `.part`, or `_tags/<tag>/` is
    provided. If two runs race, state may be corrupted; rerun to recover.

## 2. Layout

Follows the `scaffold-go-cobra` skill (see `.claude/skills/scaffold-go-cobra/SKILL.md`).

```
cmd/skopeo-image-share/main.go                  # entrypoint (slog JSON, signal ctx)
cmd/skopeo-image-share/commands/root.go         # rootCmd, Execute(ctx)
cmd/skopeo-image-share/commands/push.go         # pushCmd + runPush (leaf)
cmd/skopeo-image-share/commands/pull.go         # pullCmd + runPull (leaf)
cmd/internal/cmdsignals/signals.go              # shared signal list
cmd/internal/stdiopipe/stdiopipe.go             # cancellable std{in,out,err} pipe
pkg/skopeoimageshare/                           # one package, all library code
  skopeo.go                                     # skopeo CLI wrapper
  podman.go                                     # podman CLI wrapper
  imageref.go                                   # repo[:tag] + transport parsing
  store.go                                      # local dir-transport dump layout
  remote.go                                     # ssh.Client + pkg/sftp helpers
  transfer.go                                   # resumable blob copy
  diff.go                                       # layer-set diff calculation
PLAN.md / CODEX_REVIEW.md                       # this plan + codex review
```

Only the `push` and `pull` leaves are flat (no parent group) for now. They are
the right shape under the skill's "flat subcommand" pattern.

Rationale: start with a single `pkg/skopeoimageshare` package that owns all the
non-cobra logic. Files are split per concern (skopeo/podman wrappers, refs,
store, remote, transfer, diff) but they live in the same package so cross-cuts
are cheap. Split into sub-packages later, once the internal API boundaries
stabilize and we see actual churn. Cobra command files stay thin (flag wiring +
call into `skopeoimageshare`), per the skill's "do not too much in here" rule.

## 3. Data dir

Base: `${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share/`

Subtree, stable across runs:

```
<base>/
  <host>/<repository-path>/_tags/<tag>/            # per-image oci: dump (opaque, managed by skopeo)
  <host>/<repository-path>/_digests/<hex>/         # digest-pinned variant
  share/                                           # shared blob pool, passed via --dest-shared-blob-dir and --src-shared-blob-dir
  tmp/                                             # scratch (flat, not nested)
  log/                                             # optional ndjson run logs
```

- `<host>` = registry host portion of the ref, e.g. `docker.io`, `quay.io`,
  `ghcr.io`. For refs without an explicit host, default to `docker.io`.
  Port, when present, is kept as-is (`registry.example.com:5000`).
- `<repository-path>` = the full slash-separated path between host and tag,
  verbatim as directory components. The OCI distribution spec allows
  arbitrary depth, so this is 1+ segments.
- Tag / digest is held in a **marker directory** under the repo path, not
  mixed into a path component:
  - `<repository-path>/_tags/<tag>/` holds the tag-pinned `oci:` dump.
  - `<repository-path>/_digests/<hex>/` holds the digest-pinned dump
    (`<hex>` is the sha256 hex without the `sha256:` prefix).
- Why a marker directory (`_tags/`, `_digests/`) instead of joining tag into
  a path segment: it keeps the layout injective when one repo path is a
  prefix of another, without putting `:` into filenames.
  - `ghcr.io/a/b/c:d` → `<base>/ghcr.io/a/b/c/_tags/d/`
  - `ghcr.io/a/b/c/d:latest` → `<base>/ghcr.io/a/b/c/d/_tags/latest/`
  Both paths coexist under `a/b/c/` because `d/` (intermediate repo segment)
  and `_tags/` (marker dir) are distinct siblings.
- More examples:
  - `docker.io/library/nginx:latest` → `docker.io/library/nginx/_tags/latest/`
  - `ghcr.io/ngicks/my-tool:v1` → `ghcr.io/ngicks/my-tool/_tags/v1/`
  - `registry.example.com:5000/team/proj/sub/app:tag` →
    `registry.example.com:5000/team/proj/sub/app/_tags/tag/`
  For Docker Hub refs with no namespace (`nginx:latest`), canonicalize to
  `library/nginx` per Docker Hub's own convention.
- **Reserved names.** `_tags` and `_digests` are reserved segment names for
  this layout. The OCI distribution spec's repo-segment grammar starts with
  `[a-z0-9]`, so `_tags` / `_digests` are not legal repo segments and this
  conflict is not expected in practice. Nonetheless the CLI **rejects**
  refs whose `<repository-path>` contains a segment equal to `_tags` or
  `_digests` on input — defense in depth, and a clean failure if a
  non-compliant registry produced such a ref.
- Enumeration keys off the marker dirs: walk `<base>/<host>/.../_tags/*/`
  and `<base>/<host>/.../_digests/*/`. Any directory so reached is a tag /
  digest dump. Intermediate repo segments are just regular directories
  without markers.
- Contents under `<tag>/` are **skopeo's business** (oci: transport layout).
  We point skopeo at the directory and do not reach inside it ourselves
  except to ship it over SFTP.
- `<base>/share/` is the shared blob pool. We pass it to skopeo as
  `--dest-shared-blob-dir <base>/share` on copy so blobs are deduplicated
  across images instead of duplicated under each `<tag>/` dump.
- `<base>/tmp/` is a flat scratch dir for per-invocation temp files; callers
  must clean up what they create. No per-run UUID subdir.
- `<base>/log/` holds optional ndjson run logs.
- No `remote/` mirror: we do not cache the remote's state on disk. Remote
  enumeration is done live each run (see §4.2).
- We never delete anything implicitly; there is an explicit `prune` subcommand
  in the backlog (§9).

## 4. External commands we shell out to

We **do not** link `containers/image` Go libs — per the task description we
shell out to `skopeo` and `podman`. Wrappers live in `pkg/skopeoimageshare`
(`skopeo.go`, `podman.go`) and expose typed methods that build argv, run the
process, stream stderr to slog, and parse stdout as JSON when applicable.

### 4.1 skopeo (local and remote)

- Supported source/dest transports (user-selectable via `--local-transport`,
  `--remote-transport`):
  - `containers-storage:` (podman / CRI-O)
  - `docker-daemon:` (Docker daemon)
  - `oci:` (oci layout directory)
- On-disk exchange format: `oci:` layout **with a shared blob directory**.
  Each image dumps to `<base>/<host>/<repo-path>/_tags/<tag>/`, where skopeo writes
  only `oci-layout` and `index.json` (tiny). All blobs (manifests, configs,
  layers) are redirected to `<base>/share/` via `--dest-shared-blob-dir`. On
  the read side, `--src-shared-blob-dir` / `--shared-blob-dir` (inspect) makes
  skopeo resolve blob lookups back through `<base>/share/`. Benefits:
  - Content-addressable dedup across images is handled by skopeo itself.
  - Blob transfer decisions are made against a single pool, not per-image.
  - An `oci:<path>` load is complete from skopeo's view even when only
    missing blobs were freshly transferred, because the pool is the
    authoritative source of blob content (addresses codex §5.4.c-e).
- Key invocations:
  - Inspect a remote/source ref (manifest for blob-digest discovery):
    `skopeo inspect --raw <transport>:<ref>` -> OCI/Docker manifest JSON.
    `layers[].digest` = `sha256:<hex>`, matches blob filenames in `share/`.
  - Dump (local):
    `skopeo copy --preserve-digests --dest-shared-blob-dir <base>/share \
<src-transport>:<ref> \
oci:<base>/<host>/<repo-path>/_tags/<tag>`
  - Inspect a dumped image (tag dir):
    `skopeo inspect --shared-blob-dir <base>/share \
oci:<base>/<host>/<repo-path>/_tags/<tag>` [flag name: **verify**]
  - Load on the peer (after transfer):
    `skopeo copy --preserve-digests --src-shared-blob-dir <peer-base>/share \
oci:<peer-base>/<host>/<repo-path>/_tags/<tag> \
<dst-transport>:<ref>`
  - Version probe: `skopeo --version` on startup, remote and local.

`--preserve-digests` is important: without it, skopeo may recompress/convert
and break the digest match against the peer's manifest.

**Verify** items for this section (listed here so implementation can check
in one pass):

- Exact flag names on `inspect` / `delete`: some skopeo versions use
  `--shared-blob-dir`, some used an older spelling. Pin a minimum version.
- `--preserve-digests` interaction with `oci:` + shared blob dir (should be
  a no-op, since no conversion is requested, but verify).
- Whether skopeo writes a `blobs/sha256/` subdirectory under `<tag>/` even
  with `--dest-shared-blob-dir` set (if yes, it may be an empty stub — fine;
  just don't assume `<tag>/` stays at exactly two files).

### 4.2 Enumeration — "what does the peer already have?"

Skopeo does **not** expose an "enumerate images in this transport" operation
uniformly, so we cannot rely on skopeo alone. Enumeration is
**transport-dispatched**: each supported transport has its own discovery
strategy, run on whichever side (remote for `push`, local for `pull`) we need
to diff against.

| Transport             | Enumeration strategy                                                                                                                                                                                   |
| --------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| `containers-storage:` | `podman image ls --format json` to list refs, then `skopeo inspect --raw containers-storage:<ref>` per ref to pull the manifest (and thus blob digests).                                               |
| `docker-daemon:`      | `docker image ls --format json` (or `podman image ls --format json --storage docker` where applicable) to list refs, then `skopeo inspect --raw docker-daemon:<ref>` per ref.                          |
| `oci:` (our layout)   | We **own** the on-disk layout: refs are exactly the directories that match `<base>/<host>/<repo-path>/_tags/<tag>/`. Enumeration is a filesystem walk. Blob inventory is the filename set of `<base>/share/`. |

Important clarification: `oci:` as a skopeo transport is just "a directory
that follows the OCI image layout spec at that path." When we talk about the
`oci:` transport here, we really mean **our own disk convention built on top
of `oci:`**. That is why enumeration for `oci:` is our own logic, not
skopeo's — skopeo has no reason to know how we arrange multiple images.

Why `podman image inspect` is not used for the diff:

- `podman image inspect <ref>` exposes `RootFS.Layers` as **DiffIDs** —
  uncompressed-tarball sha256, not the compressed blob digests we need.
- `skopeo inspect --raw` on the same ref returns the manifest whose
  `layers[].digest` matches the filenames in `<base>/share/`. That is what
  the diff uses.

Result of the enumeration pass, per side:

- `peerHasBlobs: set[digest]` — union of `layers[].digest` + config +
  manifest descriptors reachable from all enumerated refs, plus the
  filename set of the peer's `<peer-base>/share/`. This is the set we
  subtract from `neededDigests` to produce `toSend` / `toFetch`.

Escape hatch: `--assume-remote-has <digest>...` (or the pull equivalent)
lets the user pre-supply the digest set and skip enumeration when they
already know what the peer has. Name it so it is clearly about raw digests,
not refs (codex nit).

Fallback when a side is missing the enumeration tool (`podman` / `docker` /
`skopeo`): the transport is unsupported, fail fast. No silent
"send-everything" fallback — the prior §11.2-vs-§4.2 contradiction is now
gone. (Remote `skopeo` is always required.)

### 4.3 ssh / sftp

- We **execute** `ssh` the binary for a connectivity probe (same codepath the
  user's `~/.ssh/config` gives them), then we dial with
  `golang.org/x/crypto/ssh` in-process for the actual session and wrap with
  `github.com/pkg/sftp`. That gives us ProxyCommand/Include/etc. support via
  `ssh -G` parsing (stretch goal; milestone 1: plain `user@host:port` +
  default `~/.ssh/id_*` + agent).
- We do not shell out for every remote command: with one `ssh.Client` we open
  multiple sessions for `podman image ls`, `skopeo inspect`, and the final
  `skopeo copy --src-shared-blob-dir <peer-base>/share oci:<tag-dir> ...`
  on load.

## 5. The push flow

Inputs (flags; exact names subject to review):

```
--image <repo:tag>                     (repeatable, or positional args)
--local-transport  containers-storage|docker-daemon|oci
--local-path <str>                     (only for oci:)
--remote-name  ssh-config-name
--remote-host  host
--remote-user  user
--remote-port  port
--remote-transport containers-storage|docker-daemon|oci
--remote-path <str>                    (only for oci:)
--data-dir   <path>                    (override XDG default)
--jobs <n>                             (per-blob parallelism; default 4)
--dry-run                              (no mutation at all — no local dump, no network, no load)
--assume-remote-has <digest>...        (skip remote enumeration; takes raw blob digests)
--keep-going                           (continue on per-image failure)
```

No `--platform` / `--remote-platform` flags: `containers-storage:` and
`docker-daemon:` refs are single-platform by construction (platform is
resolved at pull time by podman/docker), so by the time we see a ref it
is already single-platform. Multi-arch fan-out from an `oci:` index
source is out of scope — see §9.

Steps:

1. **Validate** flags, expand `$XDG_DATA_HOME`, ensure `<base>/`,
   `<base>/share/`, `<base>/tmp/`, `<base>/log/` exist.
2. **Connect** to remote, probe `skopeo --version` and `podman --version`.
   Fail fast with clear message listing missing binaries. Query the remote's
   base dir (`$XDG_DATA_HOME` resolution via ssh once).
3. **Enumerate remote** (per §4.2) and build `remoteHas: set[digest]` spanning
   all blobs skopeo could resolve from the remote's own `share/` and any
   candidate refs matching the requested images.
4. **For each image**, in sequence (images are not parallelized in v1):
   a. **Dump** (authoritative source of digests — we do **not** pre-inspect
      in the normal path):
      `skopeo copy --preserve-digests --dest-shared-blob-dir <base>/share \
  <local-transport>:<ref> \
  oci:<base>/<host>/<repo-path>/_tags/<tag>`.
      Materializes `oci-layout` + `index.json` under the tag dir and all
      blobs under `<base>/share/`. Skopeo is idempotent: re-running is
      cheap if nothing changed.
      **Dry-run branch:** skip the dump. Run
      `skopeo inspect --raw <local-transport>:<ref>` instead (read-only,
      no mutation) and use its output as the manifest JSON for step (b).
   b. **Derive digests** — normal path reads the dumped
      `<tag>/index.json` → manifest descriptor → manifest blob at
      `<base>/share/<manifestDigest>` → config + layer descriptors.
      Dry-run path parses the manifest directly from the inspect output.
      Both paths produce:
      - `manifestDigest`, `configDigest`, `layerDigests: set[digest]`.
   c. Compute
      `toSend = {manifestDigest, configDigest} ∪ (layerDigests \ remoteHas)`.
      Manifest and config blobs are **always** shipped regardless of
      `remoteHas` — they are tiny (few KB each) and unconditionally sending
      them removes a class of "peer closure is incomplete" bugs (addresses
      codex review-2 §3 concern 3).
   d. `transfer.Push(ctx, sftpClient, tagDir=<base>/<host>/<repo-path>/_tags/<tag>,
sharePool=<base>/share, blobs=toSend, remoteBase=<remote-base>)` — §6.
      Ships the tiny `<tag>/` contents wholesale (atomic tmp+rename via
      `fsutil.SafeWrite`) and the `toSend` blobs into
      `<remote-base>/share/` with resume.
      **Dry-run branch:** skip the transfer; just emit the plan (digest
      list + sizes).
   e. Run remote `skopeo copy --preserve-digests \
  --src-shared-blob-dir <remote-base>/share \
  oci:<remote-base>/<host>/<repo-path>/_tags/<tag> \
  <remote-transport>:<ref>` over an ssh session.
      **Dry-run branch:** skip; report "would load".
   f. No per-run cleanup: the remote `<tag>/` + `share/` are the persistent
      cache that the next invocation will diff against. (`prune` is backlog.)
5. Report summary: `<image> pushed (new blobs: N, reused: M, bytes: B)`.
   Dry-run prints the same summary prefixed with `DRY-RUN would:` and does
   not touch local or remote state.

Edge cases accounted for:

- Source ref is single-platform by the time we see it (see flag-surface
  note above), so no manifest-list handling is required in v1. If an
  `oci:` source happens to be a multi-arch index, skopeo's default
  platform resolution applies at dump time; `--all-platforms` fan-out is
  out of scope (§9).
- Remote has an **older tag** pointing at a different image: step (e)
  replaces the tag on the peer (**verify** overwrite semantics per
  destination transport).
- Image exists on remote with a **different digest**: load step replaces.

## 6. Resumable transfer (`pkg/skopeoimageshare/transfer.go`)

Two kinds of work:

1. **Tag-dir sync** — ship `<tag>/oci-layout` and `<tag>/index.json` (and any
   other small metadata skopeo writes under `<tag>/`) to the peer's
   `<peer-base>/<host>/<repo-path>/_tags/<tag>/`. These files are small and not
   content-addressable, so we **do not** resume them; use `fsutil.SafeWrite`
   for the atomic tmp-write + rename. On SFTP, that
   means `SafeWrite` is given an sftp-backed write factory so the same
   helper drives both local and remote writes. Done before the blob sync,
   so the peer's tag dir always refers to blobs the peer either already has
   or is about to receive.
2. **Shared-pool blob sync** — the resumable part. One goroutine pool (size
   `--jobs`), each job handles one blob:

```
func uploadBlob(ctx, sc *sftp.Client, localPath, remotePath string, expectedSize int64) error {
    part := remotePath + ".part"
    // stat
    var startAt int64 = 0
    if fi, err := sc.Lstat(part); err == nil {
        startAt = fi.Size()
        if startAt > expectedSize { // partial write grew somehow
            startAt = 0
            _ = sc.Remove(part)
        }
    }
    // short-circuit
    if fi, err := sc.Lstat(remotePath); err == nil && fi.Size() == expectedSize {
        return nil
    }

    // open local, seek
    lf, _ := os.Open(localPath); defer lf.Close()
    _, _ = lf.Seek(startAt, io.SeekStart)

    // open remote: O_WRONLY|O_CREATE, no O_APPEND so we can Seek
    rf, _ := sc.OpenFile(part, os.O_WRONLY|os.O_CREATE)
    defer rf.Close()
    if _, err := rf.Seek(startAt, io.SeekStart); err != nil { return err }

    // copy with chunked io.CopyBuffer; ctx cancel drops the conn
    // sftp supports Write+Seek per pkg/sftp docs; WriteAt also works.
    if _, err := io.Copy(rf, lf); err != nil { return err }

    // fsync-ish: close, then rename .part -> final
    rf.Close()
    return sc.Rename(part, remotePath)
}
```

Key points:

- **No app-level hashing.** Blob files are content-addressable by filename
  (= sha256 of the on-disk compressed bytes), the source file is immutable,
  and **skopeo re-verifies every blob's digest on load** (spec-mandated for
  `oci:` read path). So:
  - The only meaningful integrity check is `sha256(bytes_on_disk) ==
    filename_digest`, and skopeo already performs exactly that on read.
  - Re-deriving the digest from the logical source (e.g. recompressing
    with zstd) is **not** a valid check: compression output is not
    guaranteed to be bit-for-bit stable across tool versions or settings,
    so the re-derived digest can differ from the stored one even for
    intact content.
  - A corrupt / truncated blob surfaces as a digest-mismatch error from
    the final `skopeo copy`; the fix is a targeted re-transfer on the
    next run (after evicting the bad blob — see §9 backlog).
  Our transfer code therefore only cares about presence + size, not
  content hashing.
- Resuming from `.part` is safe because (a) the source blob is immutable
  (`sha256:<x>`) so any prefix we already wrote is a correct prefix of the
  bytes still to send, and (b) skopeo's load-time digest check is the final
  guard if that assumption ever breaks.
- Retry policy: on network error, close+recreate the `ssh.Client` and the
  `sftp.Client` and resume from `.part` size. Bounded retries (default 5,
  exp-backoff 1s..30s, `--retries`, `--retry-max-delay`).
- Progress: slog lines per blob (`started`, `resumed at=`, `completed`). A
  nicer TTY progress bar is backlog.
- Parallelism: per blob, not per image. Backpressure by channel of jobs.

## 7. The pull flow

Mirror of §5 with sides swapped. Same data-dir layout; `<base>/` here is the
_local_ base and `<remote-base>/` is the peer's.

1. Local enumeration per §4.2 (transport-dispatched) plus a walk of
   `<base>/share/`, producing `localHas: set[digest]`.
2. Per image:
   a. **Remote dump** (authoritative — we do not pre-inspect in the normal
      path): `skopeo copy --preserve-digests \
  --dest-shared-blob-dir <remote-base>/share \
  <remote-transport>:<ref> \
  oci:<remote-base>/<host>/<repo-path>/_tags/<tag>`.
      **Dry-run branch:** skip the dump. Run remote
      `skopeo inspect --raw <remote-transport>:<ref>` instead and parse the
      manifest JSON from its output.
   b. **Derive digests** from the remote `<tag>/index.json` → manifest blob
      in `<remote-base>/share/` → config + layer descriptors (dry-run uses
      the inspect output directly). Produces
      `manifestDigest`, `configDigest`, `layerDigests`.
   c. Compute
      `toFetch = {manifestDigest, configDigest} ∪ (layerDigests \ localHas)`.
      Manifest and config are always fetched regardless of `localHas`, for
      the same reason as §5.4.c.
   d. `transfer.Pull(...)` — same engine as §6, sides swapped: reads from
      remote via `sc.Open`, writes local `<base>/share/<digest>` with
      `.part` + rename resume. The `<tag>/` dir is synced via
      `fsutil.SafeWrite` (atomic tmp+rename, no resume needed).
      **Dry-run branch:** skip; emit the plan.
   e. Local load: `skopeo copy --preserve-digests \
  --src-shared-blob-dir <base>/share \
  oci:<base>/<host>/<repo-path>/_tags/<tag> \
  <local-transport>:<ref>`.
      **Dry-run branch:** skip; report "would load".
3. Summary. Dry-run prints the same summary prefixed with `DRY-RUN would:`.

Sharing the transfer engine: the helper is symmetric; we pass in an
`io.ReadSeekCloser` factory for the source and a resumable writer factory for
the destination, so push and pull are two callers of the same code.

## 8. Error handling, cancellation, logging

- All commands take `ctx` from `contextkey.WithSlogLogger` (per skill
  template) so every `exec.CommandContext` inherits cancellation.
- SIGINT/SIGTERM flip the context. Mid-blob cancellation is **per-Read**, not
  per-blob: readers passed into `io.Copy` are wrapped with
  `github.com/ngicks/go-fsys-helper/stream.Cancellable` so each individual
  `Read` call returns `ctx.Err()` the moment the context is cancelled,
  without waiting for the current blob to finish. `.part` files survive and
  the next run resumes from their size.
- Blocked SFTP **writes** (e.g. stuck in the kernel / SFTP stack on a
  dead connection) are unblocked by a forced connection close: once the
  context is cancelled, we wait a short grace period (e.g. 2 seconds) for
  in-flight operations to drain, then call `sftpClient.Close()` +
  `sshClient.Close()`, which tears down the underlying TCP connection and
  makes any pending `Write` return an error. The per-Read cancel is the
  cooperative path; the forced close is the backstop.
- We log with `slog` at:
  - `info`: per-image start/end, per-blob start/end, sizes, reuse counts
  - `debug`: argv of each skopeo/podman invocation, sftp stat/rename calls
  - `error`: non-zero exit, sftp error, digest mismatch
- We never print to stdout from library code; cobra commands write their
  summary to stdout.

## 9. Out-of-scope / backlog

Tracked in this plan so the review can comment, but **not** implemented in
milestone 1:

- `prune` subcommand (GC of `<base>/<host>/...` tag dirs and `<base>/share/`
  blobs that no tag dir references, older than N days)
- `verify` subcommand — deferred. skopeo already re-hashes blobs on `oci:`
  load, so corruption surfaces naturally. A standalone verify pass would
  only be useful for proactive disk-corruption scans, not for correctness.
- **Auto-heal on digest-mismatch** — deferred. If the final `skopeo copy`
  load fails with a digest-mismatch error, today the user has to evict the
  offending `share/<digest>` blob manually before the next run will
  re-transfer it (the "same-size corruption wedge" codex review-2 §3
  concern 4). A v2 addition would parse skopeo's error, delete the
  offending blob from `share/`, and retry once. Rare enough in practice
  (blobs become stuck only under disk bit-rot or a partial write that
  happened to land at exactly the expected size) that we accept the
  manual-eviction workaround for v1.
- Sign/verify via `cosign`
- Manifest-list / multi-arch full-fanout (`--all-platforms`)
- Parallelism across images (currently sequential)
- Keychain integration (`skopeo --authfile`) — we rely on remote being able
  to read the image from its storage directly, so auth is rarely needed;
  revisit if we add `docker://` as a supported transport.
- Using the `containers/image` Go library in-process instead of shelling out
  (smaller binary deps, better errors, but bigger surface and CGO concerns).

## 10. Test plan

- Unit:
  - `pkg/skopeoimageshare` `imageref.go` — ref parsing, `<host>/<repo-path>/_tags/<tag>`
    path derivation, transport spec formatting
  - `pkg/skopeoimageshare` `diff.go` — set math on manifest layer lists
    vs `<base>/share/` contents
  - `pkg/skopeoimageshare` `transfer.go` — resume logic against an in-process
    sftp server (`pkg/sftp` provides a server side) backed by a tempdir;
    separate tests for tag-dir sync (atomic rename) and blob sync (`.part`
    resume)
- Integration (best-effort, skipped if binaries absent):
  - `skopeo`/`podman` driven via a disposable `containers-storage` root under
    `$TMPDIR`, seeded with a tiny image (busybox-like) pulled once per CI run
  - End-to-end push & pull using `localhost` as the "remote" (ssh to self)
  - Interruption test: kill mid-transfer via context, re-run, assert `.part`
    resumed to final size and digest matches filename
- Manual smoke:
  - `containers-storage` <-> `containers-storage`
  - `docker-daemon` <-> `containers-storage`
  - `oci:` dir <-> `containers-storage`

## 11. Open questions for review

1. Is shelling out to `skopeo`/`podman` really preferred over the
   `containers/image` Go library? The library gives us typed errors and
   avoids argv assembly, at the cost of a much larger dep tree. Current plan
   honors the task description (shell out).
2. Resolved: remote `skopeo` is required (§4.2). No send-everything
   fallback; if enumeration prerequisites are missing we fail fast.
3. `.part` + rename is the whole resume scheme; no sidecar / no app-level
   hashing. skopeo's load-time digest check is the integrity backstop, so
   anything extra on top of presence+size is dead weight. Confirm this is
   acceptable.
4. Flag surface — do we want sub-sub-commands (`push image`, `push images`,
   `push resume`) or keep a single flat `push`? Current plan: single flat.
5. Remote base dir: we now use the peer's
   `$XDG_DATA_HOME/skopeo-image-share/` (resolved via
   `ssh host 'printf %s "${XDG_DATA_HOME:-$HOME/.local/share}"'`), same shape
   as local. It is a **persistent cache**, not per-run staging — the next
   invocation diffs against `<remote-base>/share/`. Confirm this is the
   intended lifetime (vs. clearing after each run).
6. `--shared-blob-dir` flag spelling on `skopeo inspect` / `skopeo delete`
   across the target skopeo versions: **verify** and pin a minimum version
   in `go.mod` / README. Owned by TODO.md task 2.0 (must complete before
   the 2.1 wrapper API is locked).
7. Resolved: `--dry-run` does **no** mutation. It replaces the local and
   remote `skopeo copy` dumps with `skopeo inspect --raw` (read-only) and
   skips the transfer + load steps. Output is the "would send / would
   fetch" plan with sizes.
