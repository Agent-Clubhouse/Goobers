# WIP takeover: ADO ephemeral pod CI polling (#7508)

Repository: `Agent-Clubhouse/Goobers`. Task worktree: `task-4/worktrees/ado`.
Branch: `codex/ado-pod-ci-poll-7508`.
Baseline: `395cf74f45765173ccb2e092aa11f57a1477c019`.
Implementation head: `725bd68111ac8f1149d2789788314ea5bb4c9004`.
The commit containing this note is the takeover checkpoint; use `git rev-parse HEAD` for its SHA.

## Status and scope

The bounded implementation is committed and independently reviewed. This is a WIP takeover checkpoint: full CI and live qualification are outstanding. Mason authorized committing and pushing work for another owner to take over. There is no merge or release approval, and no PR was created by this lane.

- Reuses the existing repository stamp, stage factory, credential plane, ADO provider and native executor for ephemeral pod polling.
- Keeps delivered token, scheme and expiry atomic; pins scheme for an attempt; refreshes safely; refuses expired or invalidated cache values and malformed/deterministically refused deliveries. Registers raw and encoded credential forms for scrubbing.
- Checks requested and returned PR scope, including supported project/repository name and ID routes. Rejects incomplete policy responses while accepting an explicit empty policy list.
- Requires explicit provider metadata and ADO broker credentials before construction; no ambient-auth fallback.
- Forwards `noVerdict`. Explicitly refuses nonempty `carryOutputs` and enabled failed-check reruns in pods, including GitHub pods that previously ignored these inputs.
- Documents behavior and self-runner rollback in `docs/guides/ado-authentication.md`.

No delegated-auth/session redesign, ADO native merge-review verdict capability, successful-response shared-quota accounting, live credentials/grants, infrastructure or enforcement changes are included. Held campaign branches were not assumed landed.

## Validation already performed

All heavy checks used the shared campaign bounded runner. Tests use fake clocks and local HTTP fixtures, not live ADO or Microsoft identity.

| Command | Result |
| --- | --- |
| `go test -race ./providers -count=1` | Passed, 16.851s at `9a8a2ead9f0f13e5e6828677a4e052bee2cc1703`; provider source and tests are unchanged at implementation head |
| `go test -race ./cmd/goobers -run 'Test.*CIPoll' -count=1` | Passed, 4.298s at implementation head |
| `make verify-fast` | Passed at implementation head: formatting, no-phone-home, vet and config-sync/goobers/operator builds |
| `go run ./test/complexitygate` | Passed at implementation head: 185 functions at the 185 budget |
| `go run ./test/cmdgoobersgrowth -base-ref 395cf74f45765173ccb2e092aa11f57a1477c019 -head-ref HEAD` | Passed at implementation head: production lines -6, production files +0 |
| `git diff --check <baseline> HEAD` | Passed at implementation head |

Independent review covered the full implementation and final fail-closed delta, with no remaining blocking findings. No new tests were run for this note-only checkpoint. No unresolved failure was observed in the listed checks. Full `make ci`, hosted CI and real stage-pod qualification were not run by this lane. The campaign owner previously reported baseline CI fix #7545 as open; confirm its current status before diagnosing broader failures.

## Evidence and next steps

Detailed evidence is preserved outside git under the original task's `campaign/` directory: `receipts/ado-final.md`, `receipts/ado-local-status.json`, `reviews/ado-independent-root.md`, `reviews/ado-pr-body.md`, and `evidence/ado-*.log`. Raw logs are intentionally not committed.

1. Inspect this checkpoint and the independent review; confirm the current base and integration changes without assuming held work has landed.
2. Coordinate full repository CI with the campaign owner. Preserve the intentional compatibility tightening for incomplete policy responses and unsupported pod inputs.
3. If preparing a PR, use the local draft as context and reference #7508. Obtain the required feature/merge review; this checkpoint is not merge approval.
4. Treat live identity, grants, token delivery, stage-image compatibility and distributed runtime qualification as separate authorized work before deployment. The existing self-runner path is the documented rollback.

No task-owned ADO tests, browser sessions, dev servers or watchers remain running at handoff. The isolated worktree and all local evidence are retained.
