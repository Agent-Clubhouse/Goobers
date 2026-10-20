The ADO selection adapter now supplies workflow ownership, exclusion-label,
opt-in, and assignee checks before policy hydration. Those checks use existing
command-level selection policy and belong beside pr-select. The provider keeps
list pagination, eligibility metadata projection, and selective policy hydration in
its reusable package; the command does not introduce a second policy evaluator.

Full webhook scans apply that same filter when listing the target's other base,
so the merged snapshot retains fresh policy state. Poll refreshes also clear the
list-body incompleteness flag because ADO detail responses carry the full body.
