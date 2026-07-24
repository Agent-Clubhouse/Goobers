# Goobers `config` repo — reference layout

This directory is a **reference example** of a Goobers `config` repo: the workforce
as code that a deploy/reconcile drives into a running instance (config-as-code,
`docs/requirements/config-as-code.md`). It includes both the linear, versioned
`quickstart` onboarding template and production-oriented workflow examples.
Quickstart is explicitly not production-safe; see the
[quickstart guide](../docs/guides/quickstart.md) for its omissions and upgrade
path.

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
| `Workflow` | State machine of tasks + gates | `triggers[]`, `start`, `tasks[]`, `gates[]`, `docsRoots[]` |

`Task` and `Gate` are **states within a `Workflow`** (not standalone objects),
matching the spec model ("a Task/Gate is a state in a workflow").

### Documentation roots (`spec.docsRoots`)

The docs-updater workflow (epic #472) keeps a project's in-repo documentation
current as the code moves. `spec.docsRoots` declares **which repo-relative paths
are documentation** — the ordered set of files/directories that workflow is
responsible for, and the only paths its run may write to:

```yaml
apiVersion: goobers.dev/v1alpha1
kind: Workflow
metadata:
  name: docs-updater
spec:
  gaggle: acme-web
  triggers:
    - type: schedule
      schedule: "30 5 * * *"
  start: gather-churn
  # In-repo documentation roots this workflow keeps current. Ordered,
  # repo-relative, files or directories. The signal-gather stage groups code
  # churn by whether it touched a declared root, and the write boundary confines
  # the run's PR to these roots — a docs run can never touch code.
  docsRoots:
    - docs
    - docs/design
    - README.md
    - ARCHITECTURE.md
  tasks:
    - name: gather-churn
      type: deterministic
      goal: Report code churn since docs were last refreshed.
      run:
        command: ["goobers", "docs-churn", "--since", "168h", "--buffer-multiplier", "3"]
      inputs:
        resultFile: "docs-churn.json"
      # ... an agentic docs stage + open-pr (with confineToDocsRoots:"true",
      # docsRoots wired from spec.docsRoots) follow — the capstone (#1018).
```

Each root must be **non-empty, repo-relative, and inside the repository** —
`goobers validate` rejects an empty, absolute, escaping, or non-existent root
with a clear message. Roots are validated but have **no default**: a workflow
without `docsRoots` simply declares no documentation surface (only the
docs-updater workflow needs them). The signal-gather stage's own knobs default
to a `168h` first-run/floor window and a `3×` buffer multiplier.

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
