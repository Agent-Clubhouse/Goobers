---
name: goobers-run-operator
description: Inspect a local Goobers instance, runs, journals, and escalations using read-only commands without changing scheduler or provider state.
---

# Goobers run operator

Diagnose local instance and workflow behavior without mutating it. Resolve the
environment with `goobers-environment-resolver` first.

## Read-only procedure

1. Identify the instance root containing `instance.yaml`, `config/`,
   `gaggles/`, and `scheduler/`. Do not infer one from a target repository.
2. Start with `goobers status --json <instance-root>` or `goobers runs list
   --json <instance-root>` to identify workflows, run IDs, phases, and
   validation warnings.
3. Inspect a run with `goobers trace --json <run-id> <instance-root>`. Use
   `--follow` only when the user asks to observe a live run.
4. Use read-only detail commands when relevant:
   - `goobers stats --json <instance-root>` for aggregate outcomes;
   - `goobers blocked list --json <instance-root>` for learned blocks;
   - `goobers claims list --json <instance-root>` for claim leases;
   - `goobers escalations show <run-id> <instance-root>` for escalation
     evidence;
   - `goobers workflow show <workflow> <instance-root>` for the state graph.
5. Correlate conclusions with immutable run identity, ordered journal events,
   and content-digested artifact pointers. Distinguish recorded evidence from
   an inference.

Do not invoke commands that start, retry, clear, release, reset, compact,
redact, or otherwise change instance, scheduler, journal, repository, or
provider state. Do not display transcripts or raw artifacts unless the user
specifically needs them; summarize secret-safe evidence and preserve the
journal's append-only contract.
