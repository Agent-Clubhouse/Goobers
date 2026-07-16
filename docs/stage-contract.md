# The stage contract (V0)

> The interface every stage executor and the runner speak. Substrate-neutral:
> identical at every tier (ARCHITECTURE.md §5, §2 invariant 4). Version:
> `v1alpha1` (`api/v1alpha1.StageContractVersion`).

A **stage** (this doc's "stage" is the workflow/task types' "task" — the terms
are equivalent, ARCHITECTURE.md §5) is a unit the runner executes: a
deterministic command or an agentic harness invocation. Gates are a machine
state whose evaluators run with stage-execution semantics.

Three JSON envelopes carry everything, defined in Go (`api/v1alpha1/envelope.go`,
`artifact.go`) and mirrored by closed JSON Schemas (`api/schemas/*.schema.json`):

| Envelope | Schema | Direction |
|---|---|---|
| `InvocationEnvelope` | `invocation.schema.json` | runner → stage |
| `ResultEnvelope` | `result.schema.json` | stage → runner |
| `Verdict` | `verdict.schema.json` | gate evaluator → runner |
| `ArtifactPointer` | `artifact-pointer.schema.json` | shared; how stages exchange bytes |

## The load-bearing invariant

**No stage reaches into another stage's state** (§2.4). Stages exchange
**envelopes and artifact pointers only**. This is enforced *by construction*, not
by convention:

- The invocation envelope has **no field carrying an upstream stage's result
  body**. A stage consumes prior work only through `contextPointers` — read-only
  pointers into the run journal. (`envelope_test.go` fails the build if a
  `ResultEnvelope`-typed field is ever added to the invocation.)
- The schemas are **closed** (`additionalProperties: false`): an envelope
  carrying an undeclared field — e.g. a legacy `upstreamOutputs` — is a
  validation error (`testdata/envelopes/invalid/invocation-upstream-reachthrough.json`).
- `outputs` on the result envelope accepts **scalars only**; anything larger is
  an artifact, referenced by pointer. State cannot be smuggled through `outputs`.

## How a stage gets its input

The runner hands the stage an `InvocationEnvelope`:

- `goal` — what to achieve.
- `workspace` — absolute path to the fresh, isolated, disposable workspace the
  stage runs in. Repo-backed stages receive a git worktree at tiers 1–2; a
  deterministic task with `run.workspace: scratch` receives an empty directory
  and does not resolve a repository.
- `contextPointers[]` — the read-only inputs. Each is exactly one of:
  - an `artifact` (`ArtifactPointer`: journal-relative `path` + `sha256` digest) —
    upstream outputs and input snapshots; or
  - an `external` ref (`kind` + `uri`) — e.g. the issue/PR URL. Content outside the
    journal is untrusted; fetching and trusting it is the stage's job.
  - on a **repass**, also the gate's most-recent `Verdict` artifact — see
    "Repass context obligation" below.
- `capabilities[]` — the capability grants the stage's definition declares (e.g.
  `github:issues:write`). **Capability admission fails closed**: credentials for a
  capability not listed here are never materialized (§5).
- `inputs` — the stage's own static config from its definition.
- `item`, `repoRef`, `limits` — the triggering backlog item, target repo, and
  execution bounds.

## Where a stage writes its output

The stage returns a `ResultEnvelope`:

- `status` — one of `success`, `failure`, `blocked`.
- `artifacts[]` — its produced outputs. The stage writes bytes into the run
  journal (`api/v1alpha1.WriteArtifact`) and returns an `ArtifactPointer` for
  each. Downstream stages receive these as `contextPointers`.
- `outputs` — small declared **scalar** values only.
- `error` — structured failure detail (`code`, `message`, `retryable`); **required
  when `status == failure`**.
- `summary`, `metrics` — human and telemetry detail.

## Artifact passing (the A → B hand-off)

Non-scalar data moves **only** by pointer:

1. Stage A: `ptr, _ := v1alpha1.WriteArtifact(journalRoot, "artifacts/a/out.txt", data, "text/plain")`
   → returns a pointer whose `digest` commits to the exact bytes.
2. Runner: puts `ptr` into stage B's invocation as a `contextPointer`. B never sees
   A's `ResultEnvelope`.
3. Stage B: `bytes, err := ptr.Resolve(journalRoot)` — reads the artifact
   **read-only** and **verifies the digest**; a mismatch is `ErrDigestMismatch`.
   Paths that escape the journal root (absolute or `..`) are refused
   (`ErrPathEscape`). Redaction runs journal-side before digesting, so digests
   commit to scrubbed bytes (§4).

See `artifact_test.go:TestTwoStagePipelineByPointerOnly` for the end-to-end toy
pipeline.

## What the runner does on each status

| Status | Runner action |
|---|---|
| `success` | advance the state machine to the next stage/gate |
| `failure` | **Non-retryable escalate disposition first (#415):** if `error.retryable == false` **and** `error.code` is a recognized escalate code (`ISSUE_OVER_SCOPE` / `NEEDS_DECOMPOSITION`), route straight to `@escalate` (terminal `escalated`) after this one attempt — bypassing `Next` and any repass loop. Otherwise: if `Next` is a gate, advance — the gate branches on the failure (the reviewer-gate pattern); if not (a non-gate stage, terminal, or empty `Next`), the run ends `PhaseFailed`. Never run downstream stages on a failed result, never silently complete. |
| `blocked` | **finish the run `escalated`** (#544/#545) — never a pause. The blocked cause is journaled (`blocked_by_agent`, carrying `error`), the claim is released via the normal terminal path, and the driving issue is notified: if `outputs.blockedBy` names blocking issue numbers, backlog selection records the block and skips the issue until every named blocker closes (self-heals automatically, #552); otherwise the issue is parked `goobers:needs-human` (#539's convention) since there is nothing concrete for selection to key off. |

> **Non-retryable escalate disposition (#415, V0.7 ladder remediation L6 —
> `docs/design/v07-ladder-remediation.md` §3.4):** a `failure` result carrying
> `error.retryable == false` **and** a recognized escalate code (`ISSUE_OVER_SCOPE`
> / `NEEDS_DECOMPOSITION`) routes straight to `@escalate` (terminal `escalated`)
> after one attempt — bypassing the `Next` gate and the repass loop. It is the
> signal a human, or a future decomposition workflow, selects on. Without it an
> un-scopeable item the stage correctly rejected on attempt 1 re-enters the repass
> loop until the budget exhausts and terminates `aborted`, not `escalated` — the
> V0.6 ladder's over-scope-probe finding. This is a business-disposition route,
> distinct from `Task.Retry` below (which is infra-only). A recognized escalate
> code with `retryable == true`, or a `failure` with an unrecognized/absent code,
> follows the ordinary failure route above.
>
> **Reviewer sibling (#415):** at an agentic review gate whose subject is an
> **agentic** stage, a run branch with **no committed change (an empty diff)**
> fast-`fail`s on the first review — resolving the gate's own `fail` branch —
> rather than issuing needs-changes and looping repasses that can only re-observe
> the same emptiness. Mirrors the #316 identical-diff guard: both spare the
> repass budget a degenerate reviewer call. Scoped to an agentic subject so a
> deterministic subject that is not expected to commit to the run branch (e.g.
> merge-review's reviewer, which judges PRs from its stage outputs) still gets a
> real reviewer evaluation on an empty diff.
>
> **`blocked` contract (#544/#545, dependency-not-met — never punish the
> producer for using a documented status):** never repass, never pause — a
> `blocked` result finishes the run `escalated` after one attempt, exactly
> like the non-retryable escalate disposition above. Use `error.code:
> DEPENDENCY_NOT_MET` (or another descriptive code — unlike `failure`'s
> escalate codes, `blocked`'s code is not runner-matched, it's for a human
> reading the journal) and `error.message` naming what's unmet. **To name the
> specific blocking issue(s) so selection can skip and self-heal (#552),**
> set `outputs.blockedBy` to a **comma-separated string of issue numbers**
> (e.g. `"441,442"` or `"#441, #442"`) — `outputs` is scalar-only by schema
> (§"Where a stage writes its output" above), so do **not** attempt an array
> or object here; a prior live occurrence tried exactly that and was
> schema-rejected, burning a whole attempt for nothing. Omit `outputs.blockedBy`
> when the block isn't attributable to specific open issues — the driving
> issue is parked `goobers:needs-human` for a human decision instead, since
> there's nothing concrete for automatic selection to skip on.

`Task.Retry` (declared retry policy, attempt budget, backoff) governs only
**dispatch/infra errors** — a Go error returned by the executor, not a
business `failure`/`blocked` `ResultEnvelope`. Each policy-driven retry
attempt is a new journal entry, never overwritten history (§5). A business
`failure`/`blocked` result is never retried by `Task.Retry`; it is handled
per the table above.

For gates, the evaluator returns a `Verdict` (`decision` ∈ `pass` / `fail` /
`needs-changes`, plus `rationale`, `evidence[]` artifact pointers, and
`findings[]`); the gate maps the decision to a branch. A gate outcome with no
defined branch is an error, never a silent pass.

**Repass context obligation (#412).** When a gate's branch routes back to a
stage the run already dispatched (a repass — most commonly `needs-changes` →
`implement`), the runner attaches that gate's just-recorded `Verdict` as a
`contextPointer` on the repass invocation, named `<gate>.verdict`, via the
same pointer-only mechanism "Artifact passing" above describes for any other
upstream artifact — never the raw `ResultEnvelope`, never a schema change. A
repassing stage that reads the reviewer's actual rationale/findings can
address them directly, rather than re-inferring "something needs to change"
from the diff alone.

## Versioning & unknown-field policy

- The contract version is `v1alpha1` (`StageContractVersion`). The whole
  `api/v1alpha1` package + `api/schemas` set is that version.
- Schemas are **closed**: unknown fields are a validation error. This is
  deliberate — it is what makes reach-through impossible and keeps the seam tight.
- Additive or breaking changes bump the contract version rather than loosening a
  schema. Validate an envelope with `api/validate.(*Validator).ValidateEnvelope`.
