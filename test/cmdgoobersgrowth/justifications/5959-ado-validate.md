# #5959: ADOACCESS004 covers run-branch namespaces and workflow folders

Growth: about +55 non-test lines and +0 files in `cmd/goobers`, all in
`validateadoaccess.go` (plus a one-line call-site change in
`validateadobacklog.go`).

- `adoRunBranchRootsByRepo` maps each Azure DevOps `repos[]` entry to the
  run-branch roots pushed to it: the branch namespace of each gaggle whose
  project resolves to that entry, and `<namespace><workflow>/` for each of its
  workflows, and `<namespace>children/`, where published child runs push. The access read and its reporter thread those roots through, and
  the ADOACCESS004 reporter gains the message for a namespace or workflow
  folder (doc comments included).
- `adoDoneStatesByTypePointer` RFC 6901-escapes the
  `backlog.doneStates.byType/<type>` diagnostic pointer (one helper, two call
  sites).

## Could any of it live elsewhere?

The parts that can be reused already moved out:

- The scope matching lives in `providers` (`RunBranchPrefixPolicies`).
- The MCP002 config check lives in `api/validate`.
- The provider-fit rule lives in `internal/capability`, shared with the
  harness.

The root mapping has to stay here. It resolves a gaggle's project to its
`repos[]` entry through `configuredRepoForProject`, the resolver every
`cmd/goobers` credential path uses, and it feeds the network-reading
`validate --check-repos` access check, which also lives in this package.
