# The goobers-io MCP: run identity and artifact I/O for agentic stages

`goobers-io` is a generic MCP server the harness wires into every agentic
stage automatically. It exposes the stage's run identity and replaces "the
model writes a file with whatever generic editing tool it reaches for, then
reports a path it hopes is right" with a small, dedicated tool surface:
`get_run_info`, `publish_output`, `list_inputs`, `read_input`, and `grep_input`.

## Why it exists

Before this existed, a stage that needed to produce a rich, freeform
artifact (a findings report, a generated doc, anything too large or
unstructured for a scalar `outputs` field) had the model write it with
`apply_patch`/`bash`/whatever tools happened to be available, then
self-report the path. Under `maxConcurrentBranches > 1` — several agentic
stages writing in parallel — that path was nondeterministic: which tool
the model reached for, whether it wrote the file before or after other
housekeeping, whether it got the relative path right, all varied run to
run. `goobers-io` fixes this by giving the model exactly one blessed way to
produce its output, and by having the harness itself resolve, validate, and
lift the result — the same discipline the completion contract already
applies to `.goobers/result.json`, extended to cover the artifact file
too.

The read side exists for the same reason from the other direction: an
upstream artifact can be arbitrarily large, and stuffing it into the prompt
as inlined JSON (the way `inputsFrom`-mapped scalar outputs work) doesn't
scale. `list_inputs`/`grep_input`/`read_input` let a stage page through or
search a large upstream artifact instead of either truncating it or
blowing the context budget.

## How auto-wiring works — there is no YAML declaration for this

`goobers-io` is **not** something a goober or workflow declares in
`tools:` or anywhere else. Every agentic stage gets `get_run_info`. The
file operations apply when either of these conditions is present:

- The stage declares an `artifactFile` input (see below) — makes
  `publish_output` available.
- The stage has any materialized upstream context (`ContextPointers`,
  populated automatically — see "How context propagates" below) — makes
  `list_inputs`/`grep_input`/`read_input` available.

A stage with neither still gets run identity access. File operations fail
closed when their corresponding output or input is absent.

**Do not** try to wire this by hand — don't add an `mcpServers` entry named
`goobers-io`, don't add its tool names to `tools:`. Doing so would route it
through the validation built for genuinely external, possibly-untrusted MCP
servers (credential isolation, `COPILOT_HOME` scoping) — checks that are
correct for that case and actively wrong for a server the harness
constructs itself with no credentials of its own. The auto-wiring exists
specifically so no goober or workflow author ever needs to think about
this; if a stage isn't getting the tools you expect, the fix is to check
whether it actually has an eligible `artifactFile` input or upstream
context, not to declare the server manually.

## Declaring the write side: `artifactFile`

Add `artifactFile` under a task's `inputs:`, naming a workspace-relative
path:

```yaml
- name: review-security
  type: agentic
  goober: quality-researcher
  goal: Review the repository through a security lens.
  workspace: repo-readonly
  inputs:
    artifactFile: findings.md
  capabilities:
    - agent:model
  expectedOutputs:
    - findingsRef
  next: "@join"
```

This does three things: it makes the stage eligible for `publish_output`;
it tells the harness which workspace-relative path to lift into a real
`ArtifactPointer` once the stage completes (the same lift that has always
existed — `artifactFile` isn't new, only the auto-wired write path is);
and, because the resulting artifact pointer propagates to the next stage
automatically (see below), it's usually the only YAML change a fan-out
lens needs to make its output actually readable downstream.

The corresponding instructions.md should tell the model to call
`publish_output` with its complete output when done — direct, explicit
naming. A tool being available is not enough on its own; without being
told to use it by name, a model defaults back to `apply_patch`/`bash`
every time. See `reference-workflows/gaggles/goobers/goobers/quality-researcher/instructions.md`
for the canonical phrasing.

## How context propagates — there is no `context:` YAML key

Unlike `artifactFile`, there is nothing to declare to get the read side
working for a downstream stage. Any `ArtifactPointer` a stage produces
(via a lifted `artifactFile`, same mechanism either way) is automatically
turned into a `ContextPointer` and threaded into the *next* stage's
envelope — linear `next:` and parallel fan-in (`@join`) both propagate
this way, with no per-branch or per-stage opt-in. For a parallel join
specifically, every branch's terminal-stage pointers accumulate into the
join stage's envelope in branch-declaration order — deterministic
regardless of which branch the runner happened to schedule first.

The practical effect: give every stage in a fan-out branch an
`artifactFile`, and the join stage downstream is automatically eligible
for `list_inputs`/`grep_input`/`read_input` over every branch's full
output — no other wiring required. `contextFrom` exists as an *allowlist*
if a stage needs to filter which accumulated pointers it receives, but the
default (omitted) is "receive everything accumulated so far," which is
almost always what you want.

Because propagation is automatic and unconditional, the same discipline
applies in reverse: an `artifactFile` on a stage feeding a *linear* next
stage propagates just as eligibly as one feeding a parallel join. A
terminal stage that reads a single upstream artifact (not a fan-in) gets
the read tools exactly the same way.

## The tools, briefly

- **`get_run_info()`** — returns the current stage's `runId`, `workflowId`,
  `taskId`, and `gaggle` directly from its invocation envelope.
- **`publish_output(content)`** — writes `content` to the stage's declared
  `artifactFile`. Available only when `artifactFile` is declared. This is
  the *only* way a model should produce that file — instructions.md should
  say so explicitly ("do not write it yourself with any other tool").
- **`list_inputs()`** — enumerates every materialized upstream artifact by
  name, reporting size and line count so the model can decide whether to
  read a small input whole or page/search a large one.
- **`read_input(name, startLine, endLine)`** — returns a line range
  (1-based, inclusive). Omitting both bounds returns the whole input unless
  it exceeds a line cap, in which case the result comes back explicitly
  truncated (never silently cut off) with the true total line count still
  reported.
- **`grep_input(name, pattern, contextLines)`** — regex search over a named
  input, returning each match's line number plus a context window
  (`contextStart`/`contextEnd`) sized to feed straight into a follow-up
  `read_input` call. Match count is capped, explicitly truncated past that
  point, never silently dropped.

The harness auto-appends a short "## goobers-io tools" section to the
stage's prompt, with the write directive when `artifactFile` is eligible
and the read directive when context is present. `get_run_info` is
self-describing in the MCP tool list. This is the mechanism, not
something an instructions.md file needs to duplicate; hand-written
completion prose that tells the model *how* to write or read its
artifact should be removed once a stage adopts this, not layered on top
of it (a model given two contradictory instructions — the harness's
"call `publish_output`," an old instructions.md's "write it with
`apply_patch`" — doesn't reliably pick the right one).

## What not to do in instructions.md

A completion-contract "## Done" section should never tell the model to
self-report the artifact under the completion JSON's `artifacts` field.
The harness's own completion-contract text unconditionally tells the model
*not* to populate `artifacts` — the runner records and digests it,
driven entirely by the `artifactFile` declaration and the `publish_output`
call, not by anything the model writes into its own JSON. A leftover
instruction saying otherwise is inert (nothing reads a model-set
`artifacts` field) and confusing — remove it.

## When the tools are missing at runtime

A registered MCP server whose subprocess fails to start is, from the
model's side, indistinguishable from a server that was never declared: the
CLI proceeds without it and the tools simply do not exist for that whole
session. The failure then surfaces two layers away wearing an unrelated
costume — typically the agent itself reporting `blocked` with a
missing-required-tools message that says nothing about MCP (#3356).

The claude-code adapter therefore compares the servers it registered for
the invocation against the CLI's own per-server connection report and
journals a `runner.annotation` of kind `mcp-server-unavailable` for every
registered server that was not connected:

```jsonc
{"type":"runner.annotation","stage":"curate","runner":{
  "kind":"mcp-server-unavailable",
  "servers":[{"server":"goobers-io","status":"failed"}],
  "detail":"registered MCP servers were not connected at invocation; ..."}}
```

Grep a run's `events.jsonl` for `mcp-server-unavailable` first whenever a
stage reports missing tools — if it is there, the tool loss is the cause
and the stage's own message is a symptom. The annotation never changes a
run's outcome; it only names the cause. Absence of the annotation is not
proof the servers were present: it is only emitted for harnesses that
report per-server connection state (claude-code does; Copilot's session
transcript carries no equivalent), and never when no report was observed
at all.

A related and much quieter config shape is a `spec.skills` entry whose
package directory does not exist. `goobers validate` reports it as
`SKILL002`; a dangling declaration contributes nothing at runtime, so
either delete it or add the package rather than letting the warning ride.

## Security notes

- `goobers-io` carries no credential and needs none — it never touches
  `internal/mcpconfig`'s credential-isolation validation, which exists for
  genuinely external MCP servers a goober declares, not for this one.
- Every path `publish_output`/`read_input`/`grep_input` touches is resolved
  and validated against the workspace root — lexical escape, a symlinked
  ancestor (existing or not-yet-existing), and an already-symlinked leaf
  are all rejected outright rather than silently followed.
- The tools carry no elevated capability beyond ordinary file I/O scoped to
  the stage's own workspace; granting them needs no `tools:` declaration or
  SEC-030-style review, unlike `shell` or `github`.

## Where this is going

This is the first, deliberately narrow generic MCP surface — fixed tool
set, no per-workflow customization. A goober or workflow author who needs
a different, typed tool the runner materializes for their own purpose is a
different, larger feature than what's described here; if that's what you
need, don't try to bend `goobers-io` to fit it — ask first.
