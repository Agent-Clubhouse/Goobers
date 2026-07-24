# How Goobers works: desired state, not scripts

Goobers is easiest to understand as a declarative control system for an agent
workforce. You describe the workforce you want in versioned definitions. The
runtime validates those definitions, admits work under their constraints, and
records what happened.

The short version is:

1. **Definitions are desired state.**
2. **The config repository or directory is the source of truth for behavior.**
3. **Agents propose behavior changes through pull requests; they do not rewrite
   the active definitions.**

## Desired state, not scripts

An imperative automation script says, "run these commands now." It often mixes
setup, execution, state changes, and recovery in one sequence. Changing the
system usually means changing the script and running it again.

A Goobers definition says, "this workforce and workflow should exist." A
workflow still contains ordered stages, and a deterministic stage may run a
command, but the definition is not a bootstrap script that edits an installed
scheduler or agent. It is persistent input that the runtime validates,
compiles into a step-machine, and executes when a trigger is admitted.

| Imperative automation | Goobers desired state |
| --- | --- |
| A script drives one execution directly. | Versioned definitions describe a reusable workforce and workflow. |
| Control logic, permissions, and commands may be mixed together. | Triggers, stages, gates, capabilities, retries, and budgets are declared and validated. |
| Recovery is whatever the script implements. | The runtime checkpoints progress and resumes from the append-only run journal. |
| Editing runtime files can change later behavior. | Behavior changes originate in the definitions under `config/`. |

At tiers 1-2, `goobers up` loads and validates `config/` at startup. The optional
`--watch-config` mode can atomically reload valid edits. In either mode,
in-flight runs remain pinned to the workflow version they started with; a
definition change affects new runs, not the meaning of recorded history. At
tier 3, Argo CD and the operator occupy the config-delivery seam, but the
definitions keep the same shape.

## The config repository is the source of truth

`goobers init` creates one instance root, but that root contains two different
kinds of data:

```text
 Definitions: desired behavior                   Runtime: observed execution
 (version and review this)                       (inspect and retain this)

 config/                                         gaggles/<gaggle>/
 |-- manifest.yaml                               |-- runs/
 `-- gaggles/<gaggle>/                           |   `-- <run-id>/
     |-- gaggle.yaml                             |       |-- run.yaml
     |-- goobers/<goober>/                       |       |-- state.json
     |   |-- goober.yaml                         |       `-- events.jsonl
     |   `-- instructions.md                     `-- workcopies/
     `-- workflows/*.yaml                        scheduler/
                                                 telemetry.db
             |
             `-- validate -> compile -> execute ------------>
```

Keeping these areas separate provides review and containment. Definitions can
pass git, CI, and human review before the runtime loads them, while execution
output cannot quietly become the next run's instructions. The runtime reads
definitions but never rewrites them as part of executing a workflow.

The left side defines what may run: gaggles, agents, workflows, triggers,
stages, gates, capabilities, and instructions. It can be a `config/` directory
inside the instance, a checkout of a separate config repository, or a
repo-relative subtree such as this project's `selfhost/` dogfood config.

The right side records what did run:

- `gaggles/<gaggle>/runs/` holds immutable inputs, artifacts, checkpoints, and
  append-only journal events.
- `gaggles/<gaggle>/workcopies/` holds managed repository copies and isolated
  per-run worktrees.
- `scheduler/` holds scheduling decisions and the claim ledger.
- `telemetry.db` is a derived local rollup for inspection.

Runtime state is not a second place to configure the workforce. Do not edit a
run journal, scheduler record, or workcopy to change future behavior; change
the definition and let the runtime load it. Conversely, do not copy runtime
state back into `config/`.

This boundary also separates different kinds of truth. The config repository
is the source of truth for **workforce behavior**. Run journals are the source
of truth for **what Goobers did**. The target repository and backlog remain the
systems of record for **project work**.

## Agents propose; repository governance decides

Agents can produce real effects when a workflow grants the required capability:
for example, an implementation agent can commit to a run branch, and a curator
can update an issue. Those effects are constrained by the workflow and recorded
in the journal.

Changing the workforce itself has a stricter path. An autonomous agent does not
edit the active definitions in place:

```text
 agent or Tutor
      |
      v
 branch with a proposed definition change
      |
      v
 pull request against the config source
      |
      |-- validation / required CI
      `-- human or CODEOWNER review
      |
      v
 merge and config delivery
      |
      v
 new runs use the new desired state
```

The Tutor follows this same rule for self-improvement. Before its `open-pr`
stage can open a pull request, Goobers verifies that every changed path is
inside the configured config root and fails closed otherwise. In a same-repo
layout such as `selfhost/`, that root must be a non-empty subtree and should be
protected by CODEOWNERS and branch rules. A separate config repository makes
the permission boundary stronger because its credential cannot reach platform
code.

This is the trust model: agents may **propose** new behavior, but the repository
review and validation policy decides whether that behavior becomes desired
state. The runtime never silently teaches itself a new workflow.

## Glossary

| Goobers term | Familiar mental model |
| --- | --- |
| **Gaggle** | A team or bounded workforce: its project/backlog connections, goobers, and workflows. |
| **Goober** | An agent role or worker definition: instructions, harness, tools, skills, model options, and allowed capabilities. |
| **Workflow** | A declarative, versioned step-machine describing triggers, stages, gates, retries, and run conditions. |
| **Stage** | One unit of work. It is deterministic (a command or built-in operation) or agentic (a harness invocation with an explicit contract). |
| **Gate** | A decision state that branches a workflow using an automated check, agentic verdict, or human approval. A gate is not a stage. |
| **Backlog** | The external queue of candidate work, such as eligible GitHub issues. It remains a project system of record. |
| **Claim ledger** | The scheduler's durable coordination record for who holds an item lease, preventing concurrent runs from processing the same work. |
| **Reconcile** | Bring runtime execution into line with validated definitions: load and compile desired state, admit eligible work, and preserve the declared constraints. |

## What to do next

Follow the [local quickstart](../guides/quickstart.md) to initialize and run an
instance. When you are ready to change the workforce, use the
[DSL authoring guide](../guides/dsl-authoring-skill.md) and validate the
definitions before proposing them.
