# Codex Review of TODO.md

## 1. Summary

`TODO.md` is close to the right shape, but it is not yet a solid implementation sequence. It needs revision before implementation starts because several tasks hide required sub-work, a few items are ordered too late to safely inform earlier APIs, and some PLAN-mandated behaviors have no explicit task owner.

## 2. Ordering & dependencies

- `9.1` is too late. PLAN `§4.1` explicitly marks skopeo flag names and minimum-version behavior as "verify", but `2.1`, `6.2`, and `7.2` already bake those flags into wrapper APIs and orchestration. The version/flag verification should happen before or during `2.1`, not as a pre-release check.
- `4.4` is out of order relative to `5.1`-`5.4`, `6.2`, and `7.2`. A transfer engine should exist before orchestration, but `4.4` is not just engine work; it defines `Push(ctx, args)` / `Pull(ctx, args)` entrypoints and retry policy, which depends on the final orchestration boundaries from PLAN `§5`, `§6`, and `§7`. As written, it overlaps later command-flow tasks and risks being rewritten.
- `6.2` depends on functionality that is not assigned earlier as a standalone task: reading dumped `index.json`, resolving the manifest descriptor into `<base>/share/<digest>`, and deriving the full digest set per PLAN `§5.4.b`. `2.3` only covers raw manifest parsing, not OCI-layout traversal.
- `7.2` has the same hidden dependency as `6.2`, but on the remote side per PLAN `§7.2.b`.
- `2.2` says `docker.go` is optional or deferrable, but `5.2`, `5.4`, `6.1`, `7.1`, `9.2`, and the release surface still assume `docker-daemon:` is in scope for v1 per PLAN `§4.1`, `§4.2`, and `§10`. That makes the sequence internally inconsistent.
- `8.2` is misplaced if `--keep-going` is a committed CLI behavior from the start. Since `6.1` wires the flag, the behavior should land with `6.2`/`7.2`, not as late polish.

## 3. Scope-per-task

- `4.4` is too large for a single PR-scale item. It combines bidirectional orchestration, worker-pool policy, retry/backoff policy, and reconnect behavior. Split the lower-level transfer worker/retry primitives from the command-facing orchestration.
- `6.2` is too large. It effectively implements all of PLAN `§5` for push, including dump, digest derivation, diffing, transfer, remote load, and dry-run branching. That is more than one clean PR-scale commit.
- `7.2` has the same problem for pull against PLAN `§7`.
- `8.1`, `8.2`, and `8.3` are probably too small as separate end-phase tasks. They are cross-cutting concerns that should be folded into the tasks that introduce the relevant behavior, especially wrappers (`2.1`, `2.2`), transfer (`4.x`), and orchestration (`6.2`, `7.2`).
- `5.4` is too small to stand alone. The dispatcher should likely ship with whichever PR finishes `5.1`-`5.3`.
- `5.1` and `5.2` drift from PLAN `§4.2`: they only union `config.digest` and `layers[].digest`, but the plan requires `peerHasBlobs` to also include manifest descriptors reachable from enumerated refs, plus the peer `share/` filename set.

## 4. Missing work

- No task explicitly owns the OCI-layout digest-derivation algorithm required by PLAN `§5.4.b` and `§7.2.b`: read `<tag>/index.json`, resolve the manifest descriptor into `<base>/share/`, then derive `manifestDigest`, `configDigest`, and layer digests from that closure. `6.2`/`7.2` mention using it, but there is no earlier implementation task.
- `5.1` and `5.2` omit the `share/` walk that PLAN `§4.2` requires for `peerHasBlobs`. Only `5.3` includes share-pool inventory.
- `5.1` and `5.2` also omit manifest digests from enumeration, despite PLAN `§4.2` saying the result includes manifest descriptors reachable from enumerated refs.
- `6.1` does not mention `--local-path`, even though PLAN `§5` includes it for `oci:` sources. `--remote-path` is only implied inside `--remote-*`; `--local-path` is not.
- No task implements the `--assume-remote-has` behavior from PLAN `§4.2` / `§5`; `6.1` only wires the flag. The pull-side equivalent also has no explicit owner.
- PLAN `§4.3` says the tool should execute `ssh` for a connectivity probe and then use `x/crypto/ssh` + SFTP for the real session. `3.1`-`3.2` cover the latter but not the `ssh`-binary probe.
- PLAN `§6` defines configurable retry knobs (`--retries`, `--retry-max-delay`); `4.4` hardcodes defaults and there is no CLI task to expose them. If those knobs remain in scope, they need task ownership. If not, PLAN should change.

## 5. Over-specified / premature items

- `2.2` is over-specified in the wrong way: calling `docker.go` "optional" is premature if v1 still claims `docker-daemon:` support in PLAN `§4.1`, `§4.2`, and `§10`. Decide support first, then sequence tasks accordingly.
- `9.1` defers a design-shaping verification too long. Since PLAN `§4.1` treats the shared-blob inspect flag spelling and version floor as unresolved, wrapper signatures in `2.1` should not be treated as settled before that check.
- `6.4` and `7.3` assume `localhost`-SSH smoke tests are the main acceptance path, but PLAN `§10` treats integration as best-effort and also calls out transport-matrix manual smoke. These tests are useful, but they should not be the only explicit validation owners for behavior that is still unresolved in earlier tasks.
- `4.4` hardcodes reconnect/backoff policy before the error model from `3.2`, `3.3`, and real external-command failures is exercised. The policy belongs lower than API shape, but higher than release polish; right now it is bundled into a task that is already too broad.

## 6. Test-plan gaps

- No task owns tests for `EnumerateContainersStorage`, `EnumerateDockerDaemon`, or `EnumerateOCI`. PLAN `§10` expects unit and integration coverage around diff inputs, but `5.1`-`5.4` contain no parser or enumeration tests.
- No task owns tests for the PLAN `§5.4.b` / `§7.2.b` digest-derivation path from `index.json` through the manifest blob in `share/`. That is central correctness logic, and it should not be left implicit inside `6.2`/`7.2`.
- No task owns tests for `--dry-run` non-mutation behavior required by PLAN `§5`, `§7`, and clarified in `§11.7`.
- No task owns tests for the `--assume-remote-has` shortcut from PLAN `§4.2` / `§5`.
- `4.2` covers resumable upload tests, but `4.3` does not explicitly call out the corresponding pull-direction resume/interruption test expected by PLAN `§10`.
- `3.3` says the cancellation test may be skipped if no sshd is available. That is reasonable, but then there is no fallback owner for the blocked-write cancellation path required by PLAN `§8`; verify whether a narrower unit/integration seam can cover it.
- `6.4` and `7.3` only exercise a `podman`-visible success path. PLAN `§10` also calls out `docker-daemon` and `oci:` manual smoke; if those remain supported in v1, TODO should assign at least a manual verification owner earlier than release tagging.
- No task owns tests for digest-pinned refs under `_digests/<hex>/`, even though `1.1` includes digest parsing and PLAN `§3` / `§5.3` treat digest directories as first-class.

## 7. Recommendation

1. Move skopeo-version/flag verification (`9.1`) up next to `2.1`, and do not freeze wrapper APIs until PLAN `§4.1` verify items are resolved.
2. Split `4.4`, `6.2`, and `7.2` into smaller PR-scale tasks. In particular, add an explicit foundation task for OCI-layout digest derivation per PLAN `§5.4.b` / `§7.2.b`.
3. Fix enumeration scope before implementation: `5.1` and `5.2` need manifest digests and peer `share/` inventory to match PLAN `§4.2`.
4. Decide whether `docker-daemon:` is truly v1. If yes, remove the "optional/defer" language from `2.2` and assign full implementation/tests. If no, revise PLAN and TODO together.
5. Pull behavior-bearing items earlier: implement `--keep-going`, `--assume-remote-has`, and any retry CLI knobs in the same phase as command orchestration, not polish.
6. Add explicit test owners for enumeration, OCI digest derivation, dry-run no-mutation, pull-direction resume, and digest-pinned refs before implementation starts.