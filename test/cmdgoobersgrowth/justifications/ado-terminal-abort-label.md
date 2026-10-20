# ADO terminal PR abort labels (#7672)

The terminal finalizer needs a small Azure Repos adapter for its private
workItemUpdater seam. This adapter delegates additions and removals to existing
provider PR-label methods, preserving native labels endpoints, case folding and
delete-by-ID behavior. Credential injection stays at the command composition
root, which resolves provider:pr:write and the credential source's PAT/Bearer
scheme for the terminal repository. No new provider API or DSL capability is
introduced. Tests cover native HTTP routing, selection visibility and scoped
credential delivery. Durable replay enablement follows the separate #7668 fix.
