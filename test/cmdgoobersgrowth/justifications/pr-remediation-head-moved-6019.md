# #6019: a run whose PR head moved under it ends superseded, not failed

Growth: +337 non-test lines and +1 file in `cmd/goobers`. Most of it is doc
comment. Most of the change itself is outside `cmd/goobers`: the engine's typed
outcome (`internal/engine`), the policy arm (`internal/escalationnotify`) and the
shared code (`internal/runner`, `internal/telemetry`).

## Why the growth belongs in the command package

- **`supersededrun.go`** (new file). This is the completed-terminal reader for
  the superseded marker. It reads the run journal, resolves the PR from the
  claim ledger or the item-repository annotations, and refunds the remediation
  charge. It is the `escalationnotify.State.SettleSupersededRun`
  implementation, beside its siblings `settleNoWorkStreak` (`noworkstreak.go`)
  and `voidRemediationChargeForRun`, which read the same stores through the
  same `cmd/goobers` seams.
- **`remediationinfravoid.go`**. The provisional-charge writer is now shared
  by the infrastructure void and the superseded refund (`markRemediationCharges`)
  instead of being duplicated.
- **`remediationcheckpoint.go` / `prretry.go`**. The `Superseded` and
  `SupersededCycles` fields on the remediation state, the refund and the
  consecutive bound at the next checkpoint, the rendered prose, and the
  operator-retry reset. That state and its decision logic already live here.
- **`dispatchdelta.go` / `dispatchcheckout.go` / `dispatchexec.go` /
  `dispatchagentic.go`**. The pod entrypoint's side of the typed refusal:
  - recording the rebound PR head the checkout started from, and carrying it
    in the stage's delta;
  - typing a refusal as `workspace_head_moved` only when the receiving
    checkout is at a different commit (`workspaceProvisionFailureCode`), so
    the run's own history rewrite stays an ordinary failure;
  - attesting that code in the surrender.

  The pod entrypoint is `goobers dispatch-exec`, a `cmd/goobers` subcommand.
  The bundle format half (`OriginRef`) lives in `internal/workspacedelta`.

## Could any of it live elsewhere?

The remediation-state type and its checkpoint decision (`remediationState`,
`decideRemediationCheckpoint`) could move into a `remediationstate` package.
The superseded reader would then follow it. That is the broader `cmd/goobers`
extraction, not this change.
