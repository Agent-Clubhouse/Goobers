The ADO selection adapter now supplies workflow ownership, exclusion-label,
opt-in, and assignee checks before policy hydration. Those checks use existing
command-level selection policy and belong beside pr-select. The provider keeps
list pagination, complete metadata projection, and selective policy hydration in
its reusable package; the command does not introduce a second policy evaluator.
