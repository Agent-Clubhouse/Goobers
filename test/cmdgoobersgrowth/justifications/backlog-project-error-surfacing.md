# ApplyBacklogProject error surfacing

Growth: +17 non-test lines and +0 files in `cmd/goobers`.

## Why the growth belongs in the command package

`providerconfig.ApplyBacklogProject` now returns its `BacklogProviderRef`
error instead of silently falling back to the routed repo. The change in
`cmd/goobers` is the three callers handling that error:

- `adoprovider.go`: the ADO stage and gaggle paths warn once and fall back to
  the routed ref, which is command-level user-facing output.
- `run_continue.go`: propagates the error to a caller that already returns one,
  so a malformed cross-provider `backlog.project` fails run continuation.

The validation itself lives in `providerconfig`; `cmd/goobers` only decides how
each command reacts to it, so none of it can move to another package.
