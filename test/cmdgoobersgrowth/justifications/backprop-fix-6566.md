# backprop/fix-6566: open-pr recovers the claimed item in stage pods

Growth: +24 non-test lines and +0 files in `cmd/goobers`, in `openpr.go` and
`claimeditem.go`.

## Why the growth belongs in the command package

- **Claimed-item lookup through the stage journal seam** (`claimeditem.go`).
  `claimedIssueFromJournal` now opens the run journal through
  `stageRunJournal`, the same seam other CLI stages use. That seam reads the
  local file journal for a self-placed run, and the dispatcher-stamped journal
  plane inside a stage pod. Previously the lookup always failed in a pod, so
  every pod-placed PR got the generic title. The function also returns why it
  found nothing.
- **Explaining the fallback** (`openpr.go`). `open-pr` threads that reason
  through title resolution and prints a warning when it falls back to
  "Automated implementation", so a generic title can be diagnosed from the
  stage log. The help text documents the pod behavior.

Both are the `open-pr` stage's own policy for naming its PR; nothing here is
reusable outside the command.

## Could any of it live elsewhere?

The reason-string plumbing could become a small result type shared by stages
that recover identity from the journal. With only `open-pr` doing this today,
a dedicated type would add indirection without shrinking the command.