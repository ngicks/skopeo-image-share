# skopeo-image-share

Share OCI images between two hosts efficiently — without standing up a registry —
by driving `skopeo` + `podman`/`docker` over an SSH connection and shipping
only the blob digests the peer doesn't already have.

## How it works

At a high level, this tool uses an OCI image layout as the exchange format.
The source and destination can still be normal local transports like
`containers-storage:` or `docker-daemon:`; the OCI directory is just the
synchronized staging format between the two hosts.

```text
LOCAL

    .-----------------------.     skopeo copy      +-----------------------------+
   / containers-storage:   /|  -----------------> | <base>/                     |
  / docker-daemon:        / |                     |   <host>/<repo-path>/       |
 / oci:                 /  |                     |     _tags/<tag>/            |
+-----------------------+   |                     |     _digests/<digest>/      |
| local image store     |  /                      |   share/sha256/<blob>       |
|                       | /                       +-----------------------------+
+-----------------------+/

                    sync missing manifests/configs/layers
    -------------------------------------------------------------------->

REMOTE

    +-----------------------------+      skopeo copy     .-----------------------.
    | <base>/                     |  -----------------> / containers-storage:   /|
    |   <host>/<repo-path>/       |                    / docker-daemon:        / |
    |     _tags/<tag>/            |                   / oci:                 /  |
    |     _digests/<digest>/      |                  +-----------------------+   |
    |   share/sha256/<blob>       |                  | remote image store    |  /
    +-----------------------------+                  |                       | /
                                                     +-----------------------+/
```

The remote side gets the same OCI directory structure as the local staging
side: per-image tag directories under `<base>/<host>/<repo-path>/_tags/` and
shared content-addressed blobs under `<base>/share/sha256/`. After sync, the
remote loads from its local OCI directory into the requested remote transport.

Each invocation:

1. Connects to a peer over SSH (default keys + `~/.ssh/known_hosts`).
2. Enumerates the peer's blob inventory:
   - `containers-storage:` → `podman image ls` + `skopeo inspect --raw containers-storage:<ref>`
   - `docker-daemon:` → `docker image ls --format json` + `skopeo inspect --raw docker-daemon:<ref>`
   - `oci:` (this tool's layout) → filesystem walk of `<base>/<host>/<repo-path>/_tags/*/`
     and `_digests/*/`, plus the filename set of `<base>/share/sha256/`.
3. Locally dumps each requested image from the configured local transport to
   an `oci:` layout with a shared blob pool (`<base>/share/`) via
   `skopeo copy --preserve-digests`.
4. Diffs the digest closure against the peer's inventory; ships
   `manifest + config` blobs unconditionally and any layer blobs the
   peer is missing. Per-blob `.part` + atomic rename gives interrupt
   resume.
5. Tells the peer to load from the synchronized OCI tag directory into its
   target transport via `skopeo copy oci:<tagDir> <transport>:<ref>` over the
   same SSH session.

`--dry-run` replaces every mutating step (local dump, network transfer,
peer load) with read-only equivalents (`skopeo inspect --raw`) and
emits a plan instead of touching state.

## Requirements

| Side   | Required binaries                                                          |
| ------ | -------------------------------------------------------------------------- |
| Local  | `skopeo`, plus `podman` or `docker` if you use those transports            |
| Remote | `skopeo` (always), plus `podman` or `docker` matching `--remote-transport` |
| SSH    | OpenSSH client on local; sftp-server on remote (standard sshd)             |

The minimum `skopeo` version must support `--shared-blob-dir` /
`--src-shared-blob-dir` / `--dest-shared-blob-dir` and
`--preserve-digests`. v1.16+ is known good.

The peer's data dir is resolved via
`${XDG_DATA_HOME:-$HOME/.local/share}/skopeo-image-share/` over SSH
once at startup.

## Known v1 limitations

- No concurrent invocations against the same `<base>/` (no locking on
  `share/`, `.part`, or tag dirs).
- No `--all-platforms` fan-out from `oci:` index sources.
- No automatic digest-mismatch eviction. If `skopeo` reports a digest
  mismatch on the peer load, manually delete the offending
  `share/sha256/<hex>` and rerun.
- No `~/.ssh/config` parsing in-process. The connectivity probe shells
  out to `ssh -G <host>` so config-derived ProxyJump/Include paths
  still work for connectivity sanity, but the in-process dial uses
  default keys (`~/.ssh/id_*`) + `SSH_AUTH_SOCK` only.

See `PLAN.md` §9 for the full backlog.
