# goobers decide stage command

`goobers decide` is a built-in stage command, so it must register in the
command layer next to every other stage command (registry, synopsis,
completion model, generated help). The command is only argument parsing,
instance-config loading and result-file writing.

The decision logic, thresholds, caching, concurrency limits and credential
handling live in internal/decisiongate and internal/decider; the command adds no
second copy of them. It is opt-in through the existing decisionGate setting and
adds no new lifecycle or result contract.
