# #6903: select-source records the post-claim parent revision

Growth: +9 net non-test lines and +0 files in `cmd/goobers`, all in
`cmd/goobers/selectsource.go`.

## Why the growth belongs in the command package

`select-source` is the stage that writes the claim comment and the
`goobers:claimed` label, so it is the only place that knows when its own writes
are finished. The change moves the provider claim marker ahead of the selection
result and re-reads the parent after it, so `Selection.Parent.ObservedRevision`
is the revision publish-slices will see when nothing else touched the issue.
The added lines are the re-read, its fail-closed claim release, and a comment
explaining why. The publisher's guard (`internal/decomposition`) is unchanged.

## Could any of it live elsewhere?

Not usefully. `ClaimWorkItem` returns no revision on every provider, so the
re-read has to happen at the call site. A shared "claim and return the
post-claim revision" helper in `providers` would be a reasonable follow-up if
another stage needs the same baseline.
