# Goobers `config` repo — reference layout

This directory is a **multi-stack production reference** for a Goobers `config`
repo: the workforce as code that a deploy/reconcile drives into a running
instance (config-as-code, `docs/requirements/config-as-code.md`). It demonstrates
production-oriented implementation, review, merge, backlog, documentation, and
policy workflows across Node.js, .NET, Java, and Python. The complete tree
contains four gaggles, 12 goobers, and 12 workflows; it is a catalog to adapt,
not a minimal starter to deploy unchanged.

For a minimal starting point, use the scaffold created by `goobers init
./my-instance` (one gaggle, one goober, and one `default-implement` workflow).
For a short guided example, use `goobers init --template=quickstart
./tutorial-instance`; that template is intentionally not production-safe. See
the [quickstart](../docs/guides/quickstart.md) for both paths.

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

## Prerequisites

Everything in this tree uses placeholder `acme/*` repositories, namespaces, and
identities. Replace those values before validation or deployment. The complete
reference assumes:

- A Goobers runner with the `goobers` binary on `PATH`, git access to each target
  repository, and schedule/webhook support for workflows that use those
  triggers.
- A configured Copilot or Claude Code agent harness, including its model
  authentication, for every agentic goober. `acme-web`'s implementer
  demonstrates `claude-code`, its other goobers and the polyglot examples use
  `copilot`; `acme-web-claude` is the same gaggle run entirely on
  `claude-code` instead (#2777's additive fleet posture — a parallel
  demonstration alongside `acme-web`, not a replacement for it). Deterministic-only
  policy workflows do not need a model.
- GitHub project and backlog connections named `github-main` and
  `github-backlog`. The example resolves `github-pat` from the `acme-kv` key
  vault; an adapted credential must grant only the capabilities listed below.
  Never put a token directly in these files.
- Preinstalled project tools. The scheduler does not install toolchains:
  `node@20` plus npm for `acme-web`, the .NET 9 SDK for `dotnet-service`, Java 21
  plus Maven for `java-service`, and Python 3.12 plus pytest for
  `python-service`. Advertise the matching token under `runner.capabilities` in
  the runtime instance's `instance.yaml`, which is operator configuration
  outside this config source tree.

## Gaggle and workflow inventory

| Gaggle | Contents | Runner and tools | Credentials |
|---|---|---|---|
| `acme-web` | Six goobers and nine workflow examples spanning implementation, backlog operations, docs, merge, and policy | `node@20`, npm, POSIX `sh`, git, Goobers; Copilot or Claude Code for agentic stages | GitHub repo and backlog connections; grants vary by family below |
| `acme-web-claude` | The same six-goober, nine-workflow shape as `acme-web`, entirely on `claude-code` (#2777's additive parallel gaggle — proves the fleet doesn't lose Copilot coverage by dogfooding Claude Code, or vice versa) | `node@20`, npm, POSIX `sh`, git, Goobers, Claude Code for agentic stages | GitHub repo and backlog connections; grants vary by family below |
| `dotnet-service` | Implementer + reviewer and `dotnet-implementation` | `dotnet@9`, .NET 9 SDK, git, Goobers, Copilot | Repository push for the implementer; no provider mutation in this focused workflow |
| `java-service` | Implementer + reviewer and `java-implementation` | `java@21`, Maven, git, Goobers, Copilot | Repository push for the implementer; no provider mutation in this focused workflow |
| `python-service` | Implementer + reviewer and `python-implementation` | `python@3.12`, pytest, git, Goobers, Copilot | Repository push for the implementer; no provider mutation in this focused workflow |

The workflow families have distinct operational prerequisites:

| Family | Workflows | Purpose and additional prerequisites |
|---|---|---|
| Starter implementation | `default-implement` | Manually claims one approved GitHub issue and delegates to `coder`. Requires the coder definition, Copilot, the project checkout, and `github:issues:write`. It illustrates the starter shape; use the generated init scaffold rather than extracting this file for a minimal install. |
| Production implementation | `implementation` | Scheduled claim, context gathering, implement/review repasses, project CI, branch push, PR creation and polling, and issue close-out. Requires `implementer` with Claude Code, `reviewer` with Copilot, the `acme-web` npm CI command, journal reads, repo push, and GitHub issue/PR write credentials. |
| Polyglot implementation | `dotnet-implementation`, `java-implementation`, `python-implementation` | Focused manual implement/review/local-CI loops. Each requires its sibling implementer and reviewer, Copilot, repo push, and its gaggle's declared runtime and CI tool. These examples deliberately omit claim, PR publication, and close-out stages, so the caller must supply the claimed work and compose a publication lifecycle if needed. |
| Backlog operations | `backlog-curation`, `backlog-assignment`, `work-nomination` | Curates and deduplicates issues, assigns approved work to a configured roster, or proposes evidence-backed work from telemetry. Curation requires `curator`, Copilot, telemetry reads, and GitHub issue/milestone/PR writes; the PR grant supports its open-PR eligibility check. Assignment is deterministic and needs GitHub issue writes. Nomination requires `nominator`, Copilot, repo and telemetry reads, and GitHub issue writes. Configure labels, roster, schedules, and rate limits before enabling. |
| Documentation | `docs-updater` | Gathers churn, confines an agentic edit to `docsRoots`, runs npm CI, pushes, and opens a PR. Requires `docs`, Copilot/model access, `node@20` with npm, repo push, and PR write credentials. Its trigger is manual until explicitly changed. |
| Merge and review | `merge-review` | Selects and reviews PRs, applies verdicts, elects a lander, merges or polls a merge queue, performs post-merge cleanup, and records refusals. Requires `reviewer`, Copilot, webhook delivery and/or scheduling, and GitHub PR review/write/merge, issue write, and branch-delete grants. Configure repository branch protection and merge-queue policy before enabling it. |
| Policy examples | `todo-check`, `inline-policy-check` | Deterministic scheduled checks requiring no provider credential or model. `todo-check` runs in a project checkout and requires POSIX `sh` plus `scripts/check-todos.sh`. `inline-policy-check` runs inline scripts in a scratch workspace, requires the runner capability `os=linux`, and does not require a checkout. |

## Copying subsets safely

Copy resources as dependency groups, then update `metadata.name`, every
`spec.gaggle`/goober reference, project and backlog coordinates, schedules,
labels, `docsRoots`, and connection references:

- A polyglot gaggle directory is a self-contained CI-loop example: copy its
  `gaggle.yaml`, both goober directories, and its implementation workflow
  together. Also provision the named connections and the declared runtime.
- For production implementation, copy `acme-web/gaggle.yaml`,
  `goobers/implementer`, `goobers/reviewer`, and `workflows/implementation.yaml`.
  Keep the gaggle's `ciCommand` aligned with the target repository. Add
  `merge-review` only with the reviewer still present and after PR/merge
  credentials, webhook or schedule delivery, and merge policy are configured.
- `docs-updater` can be added independently with `goobers/docs`; change
  `docsRoots` and its validation command together, and retain repo-push and
  PR-write grants.
- The backlog workflows are independent of implementation, but
  `backlog-curation` needs `goobers/curator` and `work-nomination` needs
  `goobers/nominator`. `backlog-assignment` is deterministic and has no goober
  dependency. Copy only the workflows whose issue mutations and schedules you
  intend to authorize.
- `inline-policy-check` is file-independent but requires a runner advertising
  `os=linux`. Copy `todo-check` only together with `scripts/check-todos.sh` into
  a project checkout with POSIX `sh`. Neither policy example needs a goober.

Do not copy a workflow alone when it names a goober, script, connection,
capability, or runtime that is absent from the destination. Run `goobers
validate --source-tree <path>` on a checked-in config source after adapting a
subset.

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
    # The shipped mechanism is inert until a maintainer explicitly activates
    # its production schedule.
    - type: manual
  start: signal-gather
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
    - name: signal-gather
      type: deterministic
      goal: Report code churn since docs were last refreshed.
      run:
        command: ["goobers", "docs-churn", "--since", "168h", "--buffer-multiplier", "3"]
      inputs:
        resultFile: "docs-churn.json"
        docsRoots: "docs,docs/design,README.md,ARCHITECTURE.md"
      next: update-docs
      # The shipped docs-updater.yaml continues through update-docs, validate,
      # push-branch, and open-pr with confineToDocsRoots:"true".
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
