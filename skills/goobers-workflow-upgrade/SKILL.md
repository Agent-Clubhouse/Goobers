---
name: goobers-workflow-upgrade
description: Assess Goobers workflows against a selected release, explain compatibility and canonical drift with source provenance, and plan or apply an explicit, validated, reviewable DSL upgrade.
---

# Goobers workflow upgrade

Assess first and write only when the user explicitly requests it. Never silently
bump a `dslVersion`, replace a custom workflow with a template, start a workflow,
materialize runtime configuration, or change daemon state.

Resolve the current environment with `goobers-environment-resolver` before doing
anything else. Resolve the selected target release separately when it differs
from the current binary. The target binary, contracts, docs, schemas, examples,
feature data, and migration tooling must all have the same exact release
identity. For a stable release use its exact tag and commit; for a pre-release or
source build use its exact commit. The default branch is never an authoritative
upgrade source.

## 1. Establish the upgrade boundary

Record these locations and identities from the resolver report:

- the config source and the active `<instance>/config` runtime copy;
- the current and target executable paths, versions, and commits;
- the current and target `dslVersions[]` support matrices;
- the verified current and target contract roots;
- whether the config source is a local directory, a local Git ref, or remote;
- every workflow file, its explicit `dslVersion`, and its owning gaggle.

Keep the effective config source, active runtime copy, and write target as
separate identities. A writable `kind: local-dir` source may be the write
target. A `kind: git` source is the committed configured ref, not whichever
branch or working tree is present at its local path, so it is analysis-only by
default. It may have a write target only when the user explicitly selects and
approves an authoring worktree whose clean `HEAD` equals the resolver's full
source commit and whose checked-out branch is exactly the configured ref.
Remote, unavailable, unresolved, or otherwise unwritable sources remain
analysis-only. The active runtime copy is evidence only unless the resolver
proves it is also the effective source.

Use a disposable scratch instance when a command requires an instance root but
the selected source is a checked-in source tree. Copy `manifest.yaml` and
`gaggles/` into the scratch instance's `config/`; do not rewrite the real source
just to satisfy a command's expected layout. Record that scratch derivation in
the report.

## 2. Collect release-matched evidence

Run commands through the exact current or target executable recorded in the
report, never an unqualified `goobers` whose identity is unknown.

1. Capture `<current> version --json`, `<current> versions --json`,
   `<target> version --json`, and `<target> versions --json`.
2. Run `<current> features --json --used <instance>` to inventory the feature
   IDs exercised by the current configuration. Run `<target> features --json
   --dsl-version <target-dsl-version>` for the target contract. If either
   optional feature command is unavailable, or `--used` cannot load an old
   config, retain its diagnostic and derive the inventory from the
   release-matched feature registry and workflow fields instead.
3. Run current validation for a baseline, then target validation against a
   scratch copy. Keep every warning and error associated with its workflow.
4. Run `<target> config diff --against <target-canonical-root>
   <scratch-instance>` with an explicit canonical root from the verified target
   release. Match workflows only by the full
   `<spec.gaggle>/<metadata.name>` identity used by `config diff`; a same-name
   workflow in another gaggle is not a peer. A workflow with no same-identity
   canonical definition is custom, not missing or obsolete.
5. Read support transitions and feature deltas from the selected release's
   machine-readable support matrix and feature output. Use exact-ref release
   notes and docs to explain semantics. An absent feature ID is not proof of
   removal: the registry retains removed features, so require an explicit
   `removed` level or a target validation diagnostic.

For each workflow, map only the feature IDs that its own fields and behavior
exercise. `features --used` returns an instance-wide union; do not attribute
every union member to every workflow. Report shared Goober features separately
when they cannot be assigned to one workflow.

The per-workflow inventory must distinguish:

- an unsupported DSL version or removed feature that prevents target loading;
- a deprecated feature that still loads but has a named replacement;
- a feature whose stability or semantics changed between the two exact
  releases;
- a feature that is unchanged and supported at the target.

## 3. Classify differences

Do not turn every canonical difference into an upgrade requirement. Put each
finding in exactly one class:

1. **Required compatibility change.** Target validation, the target support
   matrix, a removed feature, or an exact release delta proves the current form
   cannot preserve supported behavior at the target.
2. **Recommended canonical workflow improvement.** A same-identity target
   canonical workflow has a structural improvement that is not required for
   compatibility. Explain the benefit; do not apply it automatically.
3. **Local operational tuning.** `config diff` classifies these paths as
   `INFO`: trigger presence, `spec.triggers[].schedule`,
   `spec.readiness.maxConcurrentRuns`, `maxRunsPerHour`, `maxRunsPerDay`, and
   `maxOpenPRs`. No other path is tuning merely because it contains a number.
4. **User customization requiring human judgment.** Commands, goals,
   instructions, capabilities, inputs, outputs, routing, custom tasks or gates,
   and workflows with no same-identity canonical definition stay user-owned unless
   exact compatibility evidence requires a surgical change.

If evidence could fit multiple classes, required compatibility wins only when
the target source proves it. Otherwise keep the customization and identify the
review decision.

## 4. Produce a per-workflow state-graph diff

Normalize each current, target-canonical, and proposed workflow into:

- the `start` state;
- task and gate node names and kinds;
- each task's `next` edge or successful terminal;
- each gate outcome and target, including `@abort` and `@escalate`;
- parallel branch and join edges when present.

Report added and removed nodes, changed edges, changed outcome vocabularies, and
terminal changes. Ignore YAML order and formatting. Show operational trigger and
readiness differences outside the graph. For a custom workflow with no
canonical peer, show its current graph and state that no canonical graph
comparison exists; never substitute a different template.

## 5. Build the ordered upgrade plan

Plan before editing. Each step must name its workflow set, dependency, command
or surgical patch, expected diff, validation command, and a human review point.

Build a path of adjacent DSL edges for every distinct current pin. Never skip
from the oldest pin directly to the final target. Probe each direct edge with
the target binary's dry-run:

```sh
<target> fix --to <next-version> <scratch-instance>
```

When that command supports the edge, delegate the mechanical transform to
`goobers fix`; do not reproduce the transform manually. To inspect later edges,
run `--write` only inside the disposable scratch copy, validate that edge, then
continue from the resulting scratch state. When no direct migration is
registered, identify the exact missing edge and describe the smallest manual
compatibility patch from target release sources. Lower confidence accordingly.

`goobers fix` operates on every workflow in an instance. With mixed pins, do not
run `--write` on the real source if an ineligible workflow could make the command
partially apply. Use isolated scratch cohorts to obtain each mechanical diff,
then transfer only the reviewed workflow hunks to the source.

Order the plan as:

1. baseline and provenance review;
2. one adjacent compatibility edge;
3. review of the unified diff and state-graph delta;
4. validation with the target binary after that edge;
5. the next adjacent edge, repeating review and validation;
6. optional canonical improvements, each independently reviewable;
7. final target validation and targeted canonical comparison.

## Required advisory report

Return analysis before any write. Include:

1. **Scope and provenance:** current and target binary identities, exact target
   tag/ref and commit, contract source, config source, canonical root, and
   confidence.
2. **Per-workflow compatibility:** current and target pins; used feature IDs
   marked supported, deprecated, removed, or changed; validation diagnostics;
   and the evidence source for each conclusion.
3. **Per-workflow state-graph diff:** current versus target canonical and, when
   applicable, current versus proposed.
4. **Difference classification:** required compatibility changes, recommended
   canonical workflow improvements, preserved local operational tuning, and
   user customizations requiring human judgment.
5. **Ordered plan:** adjacent version edges, dependencies, dry-run or patch,
   expected file-level diff, review point, and validation after each edge.
6. **Write readiness:** `analysis-only`, `ready for explicit write`, or
   `blocked`, with unresolved evidence or decisions named.

Every recommendation must carry source provenance: release version, commit and
tag/ref when available, plus the exact command output or release-owned file that
supports it. Use:

- `high` confidence for an exact target binary plus verified same-commit
  contracts and successful machine-readable commands;
- `medium` confidence for exact verified target contracts when optional
  discovery or migration commands are unavailable;
- `low` confidence for pre-stable changes or incomplete delta metadata, even
  when pinned to an exact commit;
- `unresolved` when release identity or required target contracts cannot be
  verified. Unresolved compatibility evidence blocks the write path.

## 6. Explicit write path

Enter this section only after the user approves a specific plan and write scope.

1. Recheck that the local config source still matches the analyzed baseline.
   Preserve unrelated local changes and stop if an approved workflow changed
   after analysis. For a Git-ref source, recheck the separately approved
   authoring worktree, its exact checked-out branch, clean status, and `HEAD`
   commit; never edit another worktree merely because it points at the same
   repository.
2. Materialize the complete approved result in a scratch copy first. For every
   adjacent edge, run the target `goobers fix` dry-run, review its diff, apply it
   in scratch with `--write`, and validate before the next edge.
3. Apply only the approved workflow hunks to the config source. Preserve
   comments, user instructions, custom tasks and gates, and all allowed
   schedule/readiness tuning. Never copy an entire canonical workflow over a
   custom file.
4. After each source edit, show the file diff and normalized state-graph diff.
   Stop at the plan's review point before proceeding to the next edge or an
   optional canonical improvement.
5. Validate the final source with the target interpreter:

   ```sh
   # Initialized instance whose config/ is the source
   <target> validate --strict <instance-root>

   # Checked-in config source tree
   <target> validate --strict --source-tree <config-source>
   ```

6. Re-run the target feature inventory, targeted canonical comparison, and
   state-graph comparison. The write is complete only when validation exits
   successfully with no target validation warning, every approved change is
   present, and operational tuning plus unapproved customizations are unchanged.
7. For a Git-ref source, present the cleanly validated authoring-worktree diff
   and require a human to commit it to the configured ref. Until that commit
   exists, report the worktree as prepared for handoff, not the effective source
   as upgraded.

Do not commit, push, deploy, run `config materialize`, or restart the daemon as
part of this skill.
