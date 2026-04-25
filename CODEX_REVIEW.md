# Codex Review of PLAN.md (revision 2)

## 1. Summary

This revision is materially better than the first one on the specific staging problem it calls out: switching the exchange format from `dir:` to `oci:` with a shared blob pool is the right direction, and it removes the biggest structural flaw in the earlier design. That said, I would not ship this plan as-is. The new design introduces transport-matrix inconsistencies, an unsafe cache-healing story, and an underspecified blob-closure algorithm that can still make `oci:` loads fail even when transfer resumes work correctly.

## 2. Issues still unresolved from first review

`REVIEW_CODEX.md` is not present in this checkout, so I cannot map every first-review item. The only first-review section I can map concretely is the one explicitly referenced from `PLAN.md`.

- First review `§5.4.c-e` ("incomplete dir staging"): **fixed**.
  `PLAN.md` §4.1, §5.4.d-e, and §7.2.d-e change the exchange format to `oci:` plus a shared blob directory, so the peer-side layout is no longer inherently incomplete when only a subset of blobs is transferred on a rerun. This is the right fix for that specific flaw.

- Other first-review sections: **cannot assess from repo contents**.
  `PLAN.md` refers to the prior review, but without `REVIEW_CODEX.md` I cannot truthfully label the rest as fixed / partial / unaddressed.

## 3. New correctness concerns introduced by the revision

- **Supported transport matrix is internally inconsistent.**
  `PLAN.md` §4.1 says user-selectable transports include `containers-storage:`, `docker-daemon:`, and `oci:`. But §4.2 and §7.1 hard-code enumeration around `podman image ls` plus `skopeo inspect --raw containers-storage:<ref>`. That only matches one transport. If the destination/source is `docker-daemon:` or `oci:`, the "what does the peer already have?" logic is wrong or unavailable. This is not just inefficient; it breaks the core diffing model.

- **The data-dir mapping is not injective for multi-segment repositories.**
  `PLAN.md` §3 stores images as `<base>/<host>/<org>/<name>/<tag>/`, where `<org>` is the first path segment and `<name>` is the image name. That drops any middle path segments. Refs like `ghcr.io/acme/team1/app:1` and `ghcr.io/acme/team2/app:1` collide. The cache path must preserve the full repository path.

- **Manifest-list handling is still inconsistent with the actual blob-discovery steps.**
  `PLAN.md` §1 says single-arch only; §5 then adds `--platform` / `--remote-platform`; but §5.4.a and §7.2.a still describe blob discovery as plain `skopeo inspect --raw <transport>:<ref>`. For manifest-list refs, `--raw` may return the top-level list/index unless platform selection is applied there too (`verify`). If that happens, `neededDigests` and `toSend` are computed from the wrong object.

- **The required blob set is underspecified and easy to compute incorrectly.**
  `PLAN.md` §5.4.c and §7.2.c say "walk the `<tag>/` dir and the manifest" to find the blobs an image needs. For an `oci:` layout with shared blobs, the actual requirement is the full descriptor closure reachable from `index.json`: at minimum the manifest blob, config blob, and all layer blobs; for list/index cases, possibly more. If implementation only uses `layers[].digest` plus the tiny files in `<tag>/`, peer-side `skopeo copy ... oci:<tagdir>` will fail because the manifest blob itself is missing.

- **The "next run fixes corruption" claim is false for same-size corruption.**
  `PLAN.md` §6 says no app-level hashing is needed because `skopeo` will detect bad blobs on load and the next run can re-transfer them. That does not follow from the rest of the plan. A corrupted final blob in `<base>/share/` or `<peer-base>/share/` with the correct filename and size will still be treated as "present" by the cache walk, and `uploadBlob` short-circuits if the final file exists with `expectedSize`. The next run will fail the same way forever unless that blob is explicitly evicted.

## 4. Missing considerations

- **Concurrency / locking is not addressed.**
  The plan treats `<base>/share/`, `<base>/tmp/`, `<tag>/`, and `.part` files as globally shared state, but says nothing about concurrent invocations on the same host. Two runs targeting the same blob or tag dir can race.

- **`--dry-run` still mutates local state.**
  `PLAN.md` §11.7 says current intent is to keep the local dump even in dry-run. That is surprising for a flag named `--dry-run`, and it matters because the local cache is persistent shared state, not scratch.

- **Cancellation semantics are only described for reads, not blocked writes.**
  `PLAN.md` §8 relies on per-`Read` cancellation via `stream.Cancellable`, but push/pull can block in SFTP writes too. Unless cancellation also closes the SSH/SFTP channel, some transfers may remain stuck longer than the plan implies.

- **Remote filesystem / SFTP rename semantics need explicit verification.**
  `PLAN.md` §6 assumes tmp-write plus rename is atomic enough for tag-dir sync and blob finalization. That depends on server behavior and same-directory rename rules (`verify`).

- **Cache poisoning / stale-cache recovery is incomplete.**
  Once the persistent `share/` directory becomes an input to diffing, you need a defined remediation path for bad entries beyond "rerun". Right now the only cleanup mentioned is a future `prune` in §9, which does not solve digest-mismatch healing.

## 5. Alternative designs worth considering at this point

- **Restrict v1 transport support.**
  If the core diffing algorithm is built around `podman` plus `containers-storage:`, say so and ship that first. Supporting `docker-daemon:` and `oci:` only for final load/dump while reusing a `containers-storage:`-specific enumerator is not coherent.

- **Derive `neededDigests` from the OCI layout, not from ad hoc manifest inspection.**
  After the local/remote `skopeo copy` dump, treat `index.json` as the root and walk descriptor references in the shared blob pool. That aligns the transfer set with what the later `oci:` read path actually needs.

- **Add a targeted cache-heal path.**
  On `skopeo copy` digest-mismatch failure, identify the offending digest if possible, delete it from `share/`, and retry once. Even without app-level hashing, you need some automatic eviction mechanism.

- **Encode the full repository path in the cache path.**
  Preserve all path segments, or use a stable escaped form of the repository name. The current `<org>/<name>` scheme is too lossy.

## 6. Nit-level issues

- `PLAN.md` §2 still describes `store.go` as "local dir-transport dump layout". That comment is stale after the switch to `oci:` plus shared blobs.

- `PLAN.md` §3 says `<base>/tmp/` is "flat, not nested". With a persistent shared cache, flat tmp naming increases collision risk; even if you keep one directory, per-operation names should be clearly namespaced.

- `PLAN.md` §4.2 introduces `--assume-remote-has`, but the plan describes a digest set rather than refs. The flag name suggests image-level knowledge, not raw-blob knowledge.

- `PLAN.md` §11.2 says "Current plan requires it" about remote `skopeo`, while §4.2 still mentions a warn-only fallback. The plan should state one policy.

## 7. Recommendation

1. Narrow or fix the transport story before implementation. Either make v1 explicitly `containers-storage:`-centric, or design separate enumeration/diff paths for `docker-daemon:` and `oci:` too.

2. Redesign cache keying in §3 to preserve the full repository path. The current `<org>/<name>` layout is a correctness bug.

3. Replace the vague "`<tag>` dir + manifest" blob-discovery step with a precise OCI-descriptor-closure algorithm rooted at `index.json`.

4. Add an explicit cache-healing mechanism for digest mismatches. Without that, the persistent shared blob pool can get stuck in a bad state that retries do not repair.

5. Make manifest-list/platform behavior concrete in the actual inspect/diff steps, not just in the edge-case notes. Every command that discovers required blobs needs the same platform selection semantics (`verify`).

6. Define locking or at least collision rules for concurrent runs touching the same `share/`, `.part`, and tag-dir paths.