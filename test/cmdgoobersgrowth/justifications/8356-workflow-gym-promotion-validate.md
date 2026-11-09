# Workflow Gym promotion-evidence validation (#8356)

`goobers workflow-gym promotion-validate <evidence> <policy>` adds one
subcommand to the existing `workflow-gym` group. Loading the evidence, policy,
and referenced evaluation reports, report digest binding, last-known-good and
manifest provenance checks, sample floors, verdict derivation, and receipt
encoding all live in `internal/workflowgym`.

The command package keeps only the help text, argument-count check, and the
mapping from validation verdict to the same exit codes as
`workflow-gym evaluate` (0 pass, 1 fail, 2 usage, 3 inconclusive, 4 input
error). The verdict-to-exit mapping is shared with `evaluate` rather than
duplicated.
