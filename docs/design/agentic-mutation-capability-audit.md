# Agentic mutation-capability audit

> Status: **historical — completed survey** (2026-07-29)
>
> Scope: shipped workflow definitions under `config-examples/` and `reference-workflows/`
> at commit `45b935a89`.

## Method and classification

This audit covers every task with `type: agentic` and every gate with
`evaluator: agentic`. For an agentic task, the granted capabilities are the
task's `capabilities`; the goober definition is an upper bound, not an
additional task grant. For an agentic gate, `AgenticGate` has no capability
field, so the runner grants the referenced goober's declared capabilities
(`Runner.GateGooberCapabilities`).

Capabilities are marked **M** when they can change repository or provider
state:

- **M**: `repo:push`, `github:issues:write`,
  `github:milestones:write`
- Read-only: `repo:read`, `telemetry:read`, `journal:read`
- Service access, but not project-state mutation: `agent:model`
- `[]`: no declared capability

The inventory does not infer authority from a goal, tool name, or a goober's
unused upper-bound grant. For example, both nominator definitions permit
`github:issues:approve` at the goober boundary, but neither shipped task opts
into it, so it is not a granted capability below.

## Workflow coverage

The 17 shipped workflows contain 19 agentic entries (13 tasks and 6 gates).
The zeroes make the negative coverage explicit.

| Tree | Workflow | Agentic entries |
|---|---|---:|
| `config-examples/gaggles/acme-web` | `backlog-curation` | 1 |
| `config-examples/gaggles/acme-web` | `default-implement` | 1 |
| `config-examples/gaggles/acme-web` | `docs-updater` | 1 |
| `config-examples/gaggles/acme-web` | `implementation` | 2 |
| `config-examples/gaggles/acme-web` | `inline-policy-check` | 0 |
| `config-examples/gaggles/acme-web` | `merge-review` | 1 |
| `config-examples/gaggles/acme-web` | `todo-check` | 0 |
| `config-examples/gaggles/acme-web` | `work-nomination` | 1 |
| `config-examples/gaggles/dotnet-service` | `dotnet-implementation` | 2 |
| `reference-workflows/gaggles/goobers` | `backlog-curation` | 1 |
| `reference-workflows/gaggles/goobers` | `docs-updater` | 1 |
| `reference-workflows/gaggles/goobers` | `implementation` | 2 |
| `reference-workflows/gaggles/goobers` | `merge-review` | 1 |
| `reference-workflows/gaggles/goobers` | `pr-remediation` | 2 |
| `reference-workflows/gaggles/goobers` | `self-update` | 0 |
| `reference-workflows/gaggles/goobers` | `tutor` | 2 |
| `reference-workflows/gaggles/goobers` | `work-nomination` | 1 |

## Complete agentic-stage inventory

### `config-examples/`

| Workflow | Entry | Goober | Granted capabilities | Judgment |
|---|---|---|---|---|
| `acme-web/backlog-curation` | task `curate` | `curator` | **M** `github:issues:write`; **M** `github:milestones:write` | **Model reasoning is required.** Duplicate identity, stale-item disposition, issue decomposition, labels, and roadmap placement are semantic judgments. The provider calls can move to typed deterministic execution, but the stage cannot be replaced wholesale. |
| `acme-web/default-implement` | task `implement` | `coder` | `[]` | No mutation grant to convert. Turning a backlog item into code still requires model reasoning; the goal's request to open a PR does not itself grant that authority. |
| `acme-web/docs-updater` | task `update-docs` | `docs` | **M** `repo:push`; `agent:model` | **Model reasoning is required.** Selecting and writing accurate documentation changes is not rule-based. Remote push authority is mechanical and can be removed from the agent while retaining agent-authored commits. |
| `acme-web/implementation` | task `implement` | `implementer` | **M** `repo:push` | **Model reasoning is required.** Translating arbitrary acceptance criteria into code and tests is the core judgment. Publishing an exact reviewed commit is mechanical and belongs outside the agent. |
| `acme-web/implementation` | gate `review` | `reviewer` | `[]` | No mutation grant to convert. Diff evaluation and classed findings require model judgment. |
| `acme-web/merge-review` | gate `review` | `reviewer` | `[]` | No mutation grant to convert. Holistic PR and sibling-context evaluation requires model judgment; merge and verdict publication are already separate deterministic tasks. |
| `acme-web/work-nomination` | task `nominate` | `nominator` | `repo:read`; `telemetry:read`; **M** `github:issues:write` | **Model reasoning is required.** Deciding whether evidence is actionable, deduplicating semantically, and composing a useful issue are judgments. Applying the resulting bounded issue operations can be deterministic. |
| `dotnet-service/dotnet-implementation` | task `implement` | `dotnet-implementer` | **M** `repo:push` | **Model reasoning is required.** Implementing and testing an arbitrary .NET change is not mechanical. Any remote publication of the selected commit is. |
| `dotnet-service/dotnet-implementation` | gate `review` | `dotnet-reviewer` | `[]` | No mutation grant to convert. Reviewing the implementation evidence requires model judgment. |

### `reference-workflows/`

| Workflow | Entry | Goober | Granted capabilities | Judgment |
|---|---|---|---|---|
| `backlog-curation` | task `curate` | `curator` | **M** `github:issues:write`; **M** `github:milestones:write`; `agent:model` | **Model reasoning is required.** The curator decides semantic duplication, scope, staleness disposition, decomposition, and roadmap placement. Issue and milestone calls can be deterministic executors of bounded intent; the judgment cannot. |
| `docs-updater` | task `update-docs` | `docs` | **M** `repo:push`; `agent:model` | **Model reasoning is required.** Identifying and correcting documentation drift requires interpretation. The already-separate `push-branch` task demonstrates that remote publication is mechanical. |
| `implementation` | task `implement` | `implementer` | **M** `repo:push`; `agent:model` | **Model reasoning is required.** End-to-end issue implementation is open-ended. The task already commits without pushing; `push-branch` is the deterministic publication boundary to harden. |
| `implementation` | gate `review` | `reviewer` | `agent:model` | No mutation grant to convert. Independent diff review is a model judgment. |
| `merge-review` | gate `review` | `reviewer` | `agent:model` | No mutation grant to convert. The gate judges a PR in sibling context; deterministic tasks already publish its verdict and perform any merge. |
| `pr-remediation` | task `implement` | `implementer` | **M** `repo:push`; `agent:model` | **Model reasoning is required.** Resolving conflicts and addressing classed review/CI findings needs code judgment. Force-with-lease publication of the resulting exact commit is mechanical and already belongs to `push-remediated`. |
| `pr-remediation` | gate `review` | `reviewer` | `agent:model` | No mutation grant to convert. Determining whether every finding was correctly remediated requires model judgment. |
| `tutor` | task `analyze` | `analyst` | `telemetry:read`; `journal:read`; `agent:model` | No mutation grant to convert. Selecting and diagnosing a recurring process problem is model work, while the stage only emits an artifact. |
| `tutor` | task `draft-change` | `config-author` | **M** `repo:push`; `agent:model` | **Model reasoning is required.** Turning a diagnosis into one safe config or skill change requires interpretation. Branch publication and PR creation are already deterministic downstream tasks. |
| `work-nomination` | task `nominate` | `nominator` | `telemetry:read`; **M** `github:issues:write`; `agent:model` | **Model reasoning is required.** The stage judges evidence, deduplicates concepts, and writes the issue. A deterministic executor can validate and apply its bounded issue intent. |

## Deterministic-conversion candidates

There are **no sound whole-stage conversions** in the mutation-capable set.
Each of the 11 mutation-capable entries uses model judgment to author code,
documentation, configuration, triage decisions, or issue content. Replacing
one wholesale with rules would discard the behavior for which it is agentic.

There are, however, three concrete **mutation-execution extraction**
candidates. These preserve agentic judgment while moving credentialed,
mechanical application into trusted deterministic code.

| Priority | Candidate | Agentic entries covered | Deterministic boundary | TBH-1 and shadow-run relationship |
|---|---|---:|---|---|
| 1 | Remove remote push credentials from repo-authoring agents | 7 | Keep the agent in an isolated worktree to author and commit. A trusted stage validates the exact source SHA, branch namespace, ancestry/lease, and then publishes it. | This is TBH-1 migration order 2. For #1834, a shadow run retains the diff/commit as evidence but sends publication to the non-authoritative sink and materializes no push credential. |
| 2 | Route agent-authored issue mutations through typed proposals | 4 | Curators and nominators emit bounded create/edit/comment/label/state intent; a trusted executor validates target, freshness, limits, and reserved labels before applying it. | This is TBH-1 migration order 3. It is also the most important #1834 sink because these stages currently write directly to externally visible issues. In shadow mode the proposal is journaled but not applied. |
| 3 | Add a closed milestone-assignment route | 2 | The curator chooses an existing milestone; a typed executor validates the repository, issue, and milestone before assignment. | `github:milestones:write` is outside TBH-1's initial four proposal kinds, so it needs a separate follow-up after the issue-write route. #1834 must sink it alongside issue writes or shadow curation would still mutate roadmap state. |

Candidate 1 covers `update-docs`, all three shipped `implement` task shapes,
`pr-remediation/implement`, and `tutor/draft-change` across the two trees.
Candidate 2 covers both `curate` and both `nominate` tasks. Candidate 3 covers
the two `curate` tasks.

## Cross-reference to TBH-1 and shadow execution

TBH-1's migration order starts with merge/PR-write, then push, then issue
writes:

1. **Merge and PR-write:** this audit finds zero agentic holders of
   `github:pr:merge` or `github:pr:write`. The reference and example reviewers
   are mutation-free; `apply-verdict`, `merge-pr`, and queue handling are
   deterministic tasks. Therefore #1303 remains executor-routing hardening,
   not an agentic-stage conversion identified here. Its proposal boundary and
   #1304's staged-lite preview path are still the first migration slice.
2. **Push:** all seven agentic `repo:push` holders belong in one
   capability-route migration. Limiting the follow-up to implementation and
   remediation would leave docs-updater, Tutor config authoring, and the
   shipped examples on direct authority, contrary to TBH-1's requirement that
   a canonical route migrate completely.
3. **Issue writes:** both curation and nomination, in both trees, must migrate
   together. Their model-authored intent becomes proposals; claim,
   reconciliation, release, and disposition bookkeeping remains in closed
   runner-owned adapters as specified by TBH-1.

For #1834, the inventory gives the non-authoritative sink's minimum coverage:

- The four direct issue writers and two milestone writers must emit
  artifact-only proposals with no external application.
- The seven push holders may produce disposable worktree commits, but receive
  no remote credential; deterministic push/PR stages must preview or sink
  their effects.
- Reviewer gates, `tutor/analyze`, and the capability-empty starter task have
  no declared external mutation to suppress. They still run under the shadow
  version so their artifacts and verdicts can be compared.

This is an audit only. It changes no workflow, capability grant, routing mode,
credential injection, or provider operation. Each accepted extraction above
requires its own scoped implementation issue; no conversion is implemented
here.
