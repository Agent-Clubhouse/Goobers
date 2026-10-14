# #4421: structured claim-release reasons

Growth: +36 non-test lines and +0 files in `cmd/goobers`.

The claim ledger's release API now requires a reason
(`internal/claimrelease`), so every production release call site in the
command package states why it ends a claim. That is a compile-time guarantee:
no release path can silently fall back to "no reason".

## Where the lines come from

- **15 import lines.** Each command file that releases a claim now imports
  `internal/claimrelease`. Most call sites change in place and add no lines.
- **`claimHolderReleaseReason`** (`claimlifecycle.go`, about 10 lines). It
  sends a terminal holder's release either to the run-journal derivation
  (`localscheduler.TerminalReleaseReason`) or, for a backlog-reconcile
  reservation, to `reconciled`. The reservation run-ID grammar
  (`parseBacklogReconcileRunID`) belongs to this package.
- **Keeping the journal read off the hot path** (`claimlifecycle.go`, about 9
  lines). Terminal cleanup reads the run journal only when the run still holds
  a claim. The recovery sweep computes one reason per terminal run before it
  takes the instance-wide claims lock.
- **`issueCloseOutReleaseReason`** (`issuecloseout.go`, about 8 lines). It
  maps the stage's own close-out statuses to a disposition.
- **`workflowReleaseStepReason`** (`backlogquery.go`, 4 lines). One shared
  value for the explicit `--release` workflow steps.
- **Retiring a surrendered provider marker** (`backlogquery.go`, about 4
  lines). The retiring comment now carries the original release's reason from
  ledger history, such as stall-swept or force-released with its actor, rather
  than a generic `reconciled`.
- **`Reason` on the provider release requests.** The existing provider
  release requests now set `Reason`, which accounts for the remaining lines.

## What got smaller

- **Settle.** It used to append a second, duplicate `claim.released` event.
  It now records its outcome as the reason on the ledger's own release event.
- **The daemon's periodic sweep** (`up.go`). It no longer appends an aggregate,
  reasonless `claim.released`. The ledger journals each reaped lease as
  stall-swept.
- **`backlogClaimReleaseBreadcrumb`.** The command package's private copy of
  the release-comment text is gone; it uses the shared renderer.

## What lives elsewhere

Everything reusable lives outside `cmd/goobers`:

- the reason type, sanitizer and comment renderer are in
  `internal/claimrelease`;
- the provider-visible reason line grammar is documented in
  `docs/guides/comment-attribution.md`;
- the run-journal-to-reason mapping is in `internal/localscheduler`;
- the wire fields and the older-daemon fallback are in `internal/httpapi` and
  `internal/claimsclient`.
