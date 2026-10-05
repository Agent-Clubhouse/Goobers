# fix/pod-curation: pod reconcile reservations use the run's own workflow

Growth: +8 non-test lines and +0 files in `cmd/goobers`, all in
`reserveBacklogClaimReconciliation` (`cmd/goobers/backlogreconcile.go`): a
`workflowLabel` variable that defaults to the synthetic `backlog-reconcile`
label and, over the claims plane, takes `GOOBERS_WORKFLOW`, plus a four-line
comment explaining why.

## Why the growth belongs in the command package

The reservation is the stage command's own policy for how it names the claim it
files. The daemon side (`pinnedSharedClaimResolver.claimPolicy`) is unchanged: it
correctly refuses the synthetic label on a plain run id, and a regression test
pins that refusal. The only caller that was wrong is the stage command.

## Could any of it live elsewhere?

No. The label is chosen at the one `ClaimScoped` call in this function, and the
plane/file distinction is already made here (`claimsclient.Contained`).
