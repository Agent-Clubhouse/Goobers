# WIP takeover: Temporal inspection

Repository: `Agent-Clubhouse/Goobers`. Owned worktree: `temporal`.
Branch: `codex/temporal-rollout-contracts-4084`.

- Base: `395cf74f45765173ccb2e092aa11f57a1477c019`.
- Implementation and policy head: `cce59ebed36109d7c002d59d4e0c24b0c6d15068`.
- Reviewed source commit: `857afa643b219de684107aef1a94d7462ea6b28c`.
- Takeover checkpoint: the commit introducing this note; identify with
  `git log -1 --format=%H -- TAKEOVER.md` (branch HEAD at handoff).

## Status and scope

This is a WIP partial delivery toward #4084 and #4085. Two production read-only
CLI commands provide bounded native deployment routing/drainage observations
and offline exact-source runtime-contract inspection. Implementation lives in
`internal/workerrouting`, `internal/workercontract` and `internal/temporalops`;
the CLI registry, generated docs and tests are included.

There are no rollout mutations, admission enforcement, live deployments,
migrations or credential changes. Local compatibility is not remote-worker
attestation or readiness. The executable digest identifies an on-disk file,
not necessarily the running image; timestamped drainage is not permission to
retire a build. Source-less IR is unverifiable. See
`docs/guides/temporal-rollout-inspection.md` for the operator-facing boundaries.

## Validation and review

Passed on the source tree: focused workercontract/workerrouting/temporalops
tests, targeted CLI registration/help/completion/docs tests, complexity and
markdown checks, and `make verify-fast` (including repository-wide vet and all
command builds). A real temporary local Temporal CLI 1.8.2 fixture passed
ramp 15% → pause 0% → promotion inspection. It did not contact a deployed
Temporal service. Broader mixed-fleet pinning tests were not rerun.

The first exact-base cmd-growth gate failed because a shared snapshot re-pin
does not satisfy PR policy. Commit `cce59ebed` restores the baseline and adds
the required new justification. The corrected gate passed: +14 production CLI
lines and zero files. No production code, tests or generated docs changed in
that correction. Independent source review and parent review of the policy
correction found no remaining blockers; the source review did not independently
rerun the author's tests.

Full `make ci` and hosted PR CI have not been run for this branch. No new tests
were run for this documentation-only takeover checkpoint. Concise validation
receipts, gate logs and independent review evidence remain in the original
handoff workspace's `campaign/receipts/temporal-*` and
`campaign/reviews/temporal-independent-recovery.md`; raw logs are not committed.
No owned test, watcher or dev-server process remains active.

## Authorization and next steps

Mason authorized committing and pushing in-flight work for takeover. This is
not merge, deployment or release approval. No PR is created by this checkpoint.

1. Inspect the branch diff and this partial scope before resuming work.
2. Refresh the target base and resolve any intervening changes; rerun the exact
   PR-growth comparison against the chosen base and this branch's head.
3. Run the repository's full merge gate before requesting a merge. Review the
   saved focused/integration evidence and repeat checks if the source changes.
4. Treat future #4084/#4085 mutation/admission work as separate slices. They
   require a durable generation fence across all start paths, uncertain-start
   custody/reconciliation, accepted pin preservation and authoritative runtime
   compatibility publication. Do not infer those guarantees from this CLI.
