# #4421: structured claim-release reasons

Growth: +29 non-test lines and +0 files in `cmd/goobers`.

The claim ledger's release API now requires a reason
(`internal/claimrelease`), so every production release call site in the
command package states why it ends a claim. That is a compile-time guarantee:
no release path can silently fall back to "no reason".

## Where the lines come from

- **14 import lines.** Each command file that releases a claim now imports
  `internal/claimrelease`. The call sites themselves change in place and add
  no lines.
- **`claimHolderReleaseReason`** (`claimlifecycle.go`, about 10 lines). This
  routes a terminal holder's release either to the run journal derivation
  (`localscheduler.TerminalReleaseReason`) or, for a backlog-reconcile
  reservation, to `reconciled`. The reservation run-ID grammar
  (`parseBacklogReconcileRunID`) belongs to this package.
- **`issueCloseOutReleaseReason`** (`issuecloseout.go`, about 8 lines). This
  maps the stage's own close-out statuses to a disposition.
- **`workflowReleaseStepReason`** (`backlogquery.go`, 4 lines). One shared
  value for the explicit `--release` workflow steps.

The settle route got smaller. It used to append a second, duplicate
`claim.released` event; it now records its outcome as the reason on the
ledger's own release event. The command package's private copy of the
release-comment text (`backlogClaimReleaseBreadcrumb`) is gone too; it now
uses the shared renderer. The remaining lines come from setting `Reason` on
the existing provider release requests.

## What lives elsewhere

Everything reusable lives outside `cmd/goobers`:

- the reason type, sanitizer, renderers and marker parser are in
  `internal/claimrelease`;
- the run-journal-to-reason mapping is in `internal/localscheduler`;
- the wire fields and the older-daemon fallback are in `internal/httpapi`
  and `internal/claimsclient`.
