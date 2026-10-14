# #6482: pr-remediation skips a parked PR and stops starving failing CI

Growth: +180 non-test lines and +0 files in `cmd/goobers`, all in
`cmd/goobers/gatherprcontext.go`. Most of it is doc comment.

## Why the growth belongs in the command package

`gather-pr-context` is a `cmd/goobers` stage command. The selection policy the
fix changes already lives beside it, and it is shared with `update-behind-pr`
and the scheduler's remediation demand counter in the same package.

- **The claim loop** (`gatherPRContextClaimProgressable`, `gatherPRContextRanking`,
  `takeRemediationFallback`, `remediationCandidatesAfter`,
  `gatherPRContextExhaustedReason`). Before this change, a claimed candidate
  that could not progress ended the whole cycle. Now it is skipped with its
  claim released, and the behind-base fallback is consulted when every strong
  candidate was skipped.
- **`gatherPRContextForCandidate`**. This is the existing per-PR checkout and
  context load, lifted out of `runGatherPRContextCore` so the loop can run it
  once per candidate. It is not new behaviour.
- **`skippedStrongTierFallback` / `behindBaseRemediationCandidates`**. The
  existing behind-base tier, extracted from `selectRemediationCandidates` so
  the skip path can reuse it.
- **The FIFO fairness rule and its rationale** in `strongRemediationCandidates`.

## Could any of it live elsewhere?

The candidate ranking (`selectRemediationCandidates`, the strong tier and the
behind-base fallback) is pure and could move into a small `remediationselect`
package shared by its three callers. That is part of the broader `cmd/goobers`
extraction, not this liveness fix. The claim loop depends on the stage's claim
ledger, provider adapter and result-file conventions, all of which live here.
