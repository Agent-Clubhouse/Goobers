# #7366: keep ADO merge-refusal mutations on the pull request

This change adds 15 net non-test lines and no files in `cmd/goobers`.
The existing merge-review adapter now maps its shared comment and label
operations to ADO pull-request threads and labels. Without that mapping, a
pull-request number is interpreted as an unrelated Azure Boards work-item ID.

The adapter belongs beside the command-specific `remediationProvider` surface:
it translates the merge-review stages' PR-only use of that interface. The HTTP
operations remain in the existing `providers.ADOProvider` methods; the shared
ADO work-item API retains its work-item semantics. Removing the old sticky
comment special case also lets repeated refusals update the same PR thread.
