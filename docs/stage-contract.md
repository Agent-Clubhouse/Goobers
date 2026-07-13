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
- `workspace` — absolute path to the fresh, isolated, disposable working copy the
  stage runs in (a git worktree at tiers 1–2; a pod workspace at tier 3).
- `contextPointers[]` — the read-only inputs. Each is exactly one of:
  - an `artifact` (`ArtifactPointer`: journal-relative `path` + `sha256` digest) —
    upstream outputs and input snapshots; or
  - an `external` ref (`kind` + `uri`) — e.g. the issue/PR URL. Content outside the
    journal is untrusted; fetching and trusting it is the stage's job.
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
| `failure` | apply the stage's declared retry policy; when exhausted, branch on failure. Each attempt is a new journal entry, never overwritten history (§5). |
| `blocked` | halt the run pending external intervention (human input, an unmet dependency) |

For gates, the evaluator returns a `Verdict` (`decision` ∈ `pass` / `fail` /
`needs-changes`, plus `rationale`, `evidence[]` artifact pointers, and
`findings[]`); the gate maps the decision to a branch. A gate outcome with no
defined branch is an error, never a silent pass.

## Versioning & unknown-field policy

- The contract version is `v1alpha1` (`StageContractVersion`). The whole
  `api/v1alpha1` package + `api/schemas` set is that version.
- Schemas are **closed**: unknown fields are a validation error. This is
  deliberate — it is what makes reach-through impossible and keeps the seam tight.
- Additive or breaking changes bump the contract version rather than loosening a
  schema. Validate an envelope with `api/validate.(*Validator).ValidateEnvelope`.
