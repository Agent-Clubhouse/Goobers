# Goobers `config` repo — reference layout

This directory is a **reference example** of a Goobers `config` repo: the workforce
as code that a deploy/reconcile drives into a running instance (config-as-code,
`docs/requirements/config-as-code.md`). It is intentionally minimal — one gaggle,
one coder goober, and the default length-1 workflow — so it reads as a starting
point, not a kitchen sink.

## Folder layout

```
config-examples/
  manifest.yaml                         # top-level desired state (kind: Manifest)
  gaggles/
    <gaggle-name>/
      gaggle.yaml                       # kind: Gaggle
      goobers/
        <goober-name>/
          goober.yaml                   # kind: Goober
          instructions.md               # persona/behavior (markdown + frontmatter)
      workflows/
        <workflow-name>.yaml            # kind: Workflow (tasks + gates)
```

The layout is a convention for discoverability (CFG-007); config loading keys
off each object's `kind` + `metadata.name`, not its path, so you
may split or combine files as you like (multi-document YAML with `---` is
supported).

## Objects

Every object is a Kubernetes-style resource: `apiVersion: goobers.dev/v1alpha1`,
`kind`, `metadata.name`, `spec`. The canonical Go types live in `/api/v1alpha1`
and the JSON Schemas in `/api/schemas`.

| Kind | Purpose | Key spec fields |
|---|---|---|
| `Manifest` | Top-level instance desired state | `instance`, `connections[]`, `gaggles[]` |
| `Gaggle` | Siloed workforce; targets a repo + singleton backlog | `project`, `backlog`, `isolation` |
| `Goober` | Role-specialized AI worker | `gaggle`, `role`, `instructions`, `scaleFactor`, `workflows[]` |
| `Workflow` | State machine of tasks + gates | `triggers[]`, `start`, `tasks[]`, `gates[]` |

`Task` and `Gate` are **states within a `Workflow`** (not standalone objects),
matching the spec model ("a Task/Gate is a state in a workflow").

## Goober instruction format

A goober's behavior is authored as **markdown with optional YAML frontmatter**
(`GBO-002`, `CFG-003`). `goober.yaml` references the file via `spec.instructions`
(a path relative to the goober's directory).

```markdown
---
role: coder
description: One-line summary shown in tooling.
tags: [implementer]
---

# Coder

<persona, responsibilities, scope, and "done" criteria in prose>
```

- **Frontmatter** is advisory metadata (role, description, tags, optional model
  hint) for the harness/portal. The **authoritative** configuration — skills,
  tools, harness, scale, workflow association — lives in `goober.yaml`, so the two
  cannot drift.
- **Body** is the instruction prose handed to the agent harness at invocation.

## Connections & secrets

`connections[]` in the manifest declare named links to external systems; gaggles
and repos reference them by name (`connectionRef`). Credentials are **Key Vault
references** (`secretRef`), never inline tokens (`CFG-009`, `SEC-010`).

## Scaling and process — what to change next

- **More throughput:** raise a goober's `spec.scaleFactor` and redeploy → more
  concurrent replicas drawing from the shared backlog (`GBO-030`).
- **More process:** add tasks (research, tests) and gates (automated checks,
  reviewer goobers, human approvals) to a workflow. Every gate has exactly one
  evaluator — chain gates to compose checks (`GT-016`).
