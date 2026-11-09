# Workflow Gym regression evaluation (#8355)

`goobers workflow-gym evaluate [--min-samples N] [--regression-threshold RATE] <dataset> <tournament-report>` adds
one subcommand to the existing `workflow-gym` group. The command delegates
paired confidence intervals, fail-closed regression gating, provenance
digests, held-out isolation, and Pareto ranking to `internal/workflowgym`.

The command package keeps only flag parsing, report encoding, and the mapping
from gate verdict to a distinct exit code (0 pass, 1 fail, 2 usage,
3 inconclusive, 4 input or processing error), so automation can tell a
measured regression apart from missing evidence or a broken input.
