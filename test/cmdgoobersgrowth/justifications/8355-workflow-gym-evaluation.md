# Workflow Gym regression evaluation (#8355)

`goobers workflow-gym evaluate [OPTIONS] <dataset> <tournament-report>` adds
one subcommand to the existing `workflow-gym` group. The command delegates
paired confidence intervals, fail-closed regression gating, provenance
digests, held-out isolation, and Pareto ranking to `internal/workflowgym`.
