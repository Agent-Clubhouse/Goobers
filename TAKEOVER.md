# Takeover: atomic merge-review finding ledger

Status: reviewed and locally validated follow-up; not merged or rolled out.
Repository: `Agent-Clubhouse/Goobers`.
Branch: `campaign/7464-atomic-ledger`.
Existing PR: https://github.com/Agent-Clubhouse/Goobers/pull/7464
This branch continues that PR's lineage; it does not replace or duplicate the
ledger implementation. The existing PR branch has not been updated.

## Checkpoint

- Latest fetched main/base: `395cf74f45765173ccb2e092aa11f57a1477c019`.
- Original PR head: `72bd3857d2d9d09ce7df039288981988ad2e8388`.
- Main was merged normally in `19d9840c2`.
- Atomic publication fix: `fab298e22b259d2f3864cce2fcd28dabd43ceb20`.
- Reviewed and tested code head: `1fb91f4809e863f08205abc6b6a5eb77d6a5b0a0`.
- The checkpoint commit after that head adds only this note.

The follow-up folds inside scheduler-state lock/CAS retries, rejects stale
cross-head replacement using a captured predecessor, binds dispositions to
finding generations, returns the committed fold to every publisher, and commits
ADO state before its pass/non-pass transport. Complete snapshots prevent delayed
provider projections from resurrecting resolved findings. Independent review
found two replay defects; both were fixed in the reviewed code head above.

## Validation

Passed at the tested code head:

- Focused race tests for `internal/findingledger`, `api/schemas`, `api/validate`
  and command-level verdict, ADO publication, merge and healer regressions.
- `go test -tags=integration ./cmd/goobers -run '^TestIntegrationVerdictCommit' -count=1`.
- PR-relative growth against main: +417 command lines, +1 file, with the new
  `5890-undispositioned-findings.md` justification; no baseline repin.
- Complexity gate, API contract regeneration with no diff, and `make verify-fast`.
- Independent source review at the exact code head: no remaining blocker.

Full `make ci` and fresh remote CI for this branch were not run. An earlier
`make generate` failed after controller-gen emitted unrelated invalid
`time.Time.DeepCopyInto` references; that generated delta was discarded and
preserved locally for diagnosis. The new scalar fields require no deepcopy
change because the generated methods already copy `*out = *in`.

Local evidence is retained outside Git in the task workspace:
`campaign/merge-ledger-receipt.md`, `campaign/merge-pr7464-update-draft.md`,
`campaign/reviews/ledger-initial.md`, `campaign/reviews/ledger-final.md`, and
`campaign/receipts/merge-ledger-{focused,gates,generate}.log`.
These local artifacts are not reproducible from this repository.

## Constraints and next steps

All active writers for a PR/status context must move together to the atomic
publication path. After the first committed generation, generation-zero comments
cannot extend that snapshot; mixed old/new writers and rollback to the old writer
are not a transparent rollout. Native provider requests can still complete out
of order, and cross-gaggle publisher custody is not established here. Provider
records retain the existing trusted-actor/marked-comment boundary; ADO retains
its existing display-name fallback if AuthorID is absent.

Mason authorized checkpointing and pushing unfinished work for another owner.
That authorization does not authorize merging, releases, credentials, live policy
changes, or claiming unfinished work validated. No merge or live change occurred.

The next owner should inspect current main and PR #7464, preserve this reviewed
lineage, run the campaign integration gate, and decide whether to update the
existing PR normally. Do not open a duplicate ledger PR. Missing-verdict shadow,
stale-base invalidation, merge-group authority and pure-rebase carry-forward are
separate work. The partial shadow draft is checkpointed on
`campaign/7462-verdict-shadow` and is unvalidated.
