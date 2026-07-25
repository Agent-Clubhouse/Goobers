# Design: Deep-investigation workflow

> **Status:** Accepted design (2026-07-25); runtime implementation is deferred.
> **Related:** #816 (this design), #1482 (workflow and goobers), #1483 (triage
> label and routing), #1484 (evidence schema and durable emission),
> [`ARCHITECTURE.md` sections 4-5](../ARCHITECTURE.md), and
> [`stage-contract.md`](../stage-contract.md).

## 1. Decision

Goobers will provide a static workflow named **`deep-investigation`** for defects
whose fix cannot be justified before the symptom has been reproduced and measured.
It has exactly five stages:

```text
reproduce -> instrument -> implement-fix -> validate-reproduction -> emit-evidence
```

Deterministic contract gates between the stages admit only the runner-authored
artifact sets described below. A gate can advance to the next stage or terminate
at `@escalate`; it cannot invent stages, skip reproduction, or route back through
an open-ended agent loop. The workflow is therefore a pinned step-machine, not a
planner.

The load-bearing rule is:

> **The fix is not validated unless the immutable harness and symptom oracle that
> demonstrated the baseline defect run against the fixed revision and no longer
> observe that defect under the declared validation budget.**

Unit and regression tests remain useful supplementary evidence, but cannot replace
that reproduction-gated end-to-end check.

This document defines the workflow contract. It does not add the workflow, labels,
runtime artifact types, or automatic routing.

## 2. Manual triage and admission

### 2.1 When to use `deep-investigation`

A human triager applies the `hard` label and records a short triage note when the
issue is approved and at least one of these conditions holds:

- the symptom is intermittent, timing-sensitive, load-dependent, or sensitive to
  process ordering, resource pressure, clock behavior, or machine state;
- adding logging, a debugger, or ordinary test isolation changes whether the
  symptom appears (a heisenbug);
- the failure crosses process, package, service, or runtime boundaries and there
  are multiple plausible causes at different layers;
- a useful reproduction needs a purpose-built workload, fixture, fault injector,
  long-running loop, or concurrency harness rather than one ordinary test command;
- dumps, traces, profiles, scheduler histories, or equivalent captured evidence
  are needed to distinguish correlation from a causal mechanism; or
- the symptom is understood, but the correct fix altitude cannot be selected
  without evidence showing which layer owns the violated invariant.

The triage note supplies the known symptom, affected environment, available
observations, safety constraints, and a bounded investigation budget (time,
attempts, load, disk, and artifact size). Public-backlog approval remains required
under SEC-047; `hard` is not a trust label.

### 2.2 When ordinary implementation is the right route

Keep an issue in the ordinary implementation workflow when a deterministic
reproduction or failing test already exists, the responsible invariant and layer
are known, and normal targeted plus end-to-end validation can prove the requested
change. A large diff, vague acceptance criteria, missing product direction, or
several independent requests do not make an issue `hard`: those are scoping or
decomposition problems. Likewise, do not route an issue here merely to obtain more
attempts after ordinary implementation failed.

### 2.3 Initial routing

Initial routing is deliberately human-applied:

1. A triager applies `hard`, records the triage note, and prevents the ordinary
   implementation workflow from claiming the same item.
2. An operator explicitly starts `deep-investigation` for that item.
3. The workflow runs the fixed graph in section 1.

There is no classifier, inferred label, scheduler priority election, or automatic
transfer from `implementation` in this design. #1483 owns the exact label
provisioning, manual command/selector, and mutually exclusive claim behavior.
Automatic routing is deferred beyond #1483.

## 3. Run-wide investigation contract

The issue snapshot, triage note, target repository and base revision, budgets, and
environment constraints are immutable run inputs. Each stage receives only those
inputs and digest-verified context pointers from earlier stages. It runs in a fresh
disposable workspace; no server, process, temporary file, or ambient machine state
is a stage handoff.

The reproduction bundle is immutable after `reproduce`. Its digest is carried
through every later result. `instrument` may copy and exercise the bundle, and may
temporarily modify its own worktree to add probes, but it cannot replace the bundle.
`implement-fix` changes product source on the run branch; it cannot edit the
journaled reproduction. `validate-reproduction` fails closed if the bundle or
symptom-oracle digest differs from the baseline.

All non-scalar data crosses stages as runner-authored `ArtifactPointer` values. The
implementation commit crosses through the run branch because only committed branch
state survives fresh worktrees. Transcripts and telemetry spans remain diagnostic;
a required handoff must be a declared artifact, not something a later stage scrapes
from a transcript.

### 3.1 Runner-authored artifact-set handoff

The shipped agentic harness can lift only one task-declared `inputs.artifactFile`
and exposes it downstream as `<stage>.artifact[0]`. Agentic completion envelopes
cannot be trusted to mint their own paths or digests, and the current runner does
not create semantic context names such as `reproduction.bundle`. That single-file
surface is insufficient for a harness plus a variable set of dumps, traces, or
profiles.

Therefore #1484 must add **manifest-driven agentic artifact-set lifting** before
#1482 implements this workflow. Each deep-investigation task declares:

```yaml
inputs:
  artifactManifestFile: .goobers/deep-investigation-artifacts.json
```

The stage writes that workspace-relative staging manifest with this logical shape:

```yaml
schemaVersion: goobers.dev/stage-artifact-set/v1alpha1
entries:
  - name: reproduction.bundle
    path: .goobers/out/reproduction-bundle.tar
    mediaType: application/x-tar
  - name: reproduction.baseline
    path: .goobers/out/reproduction-baseline.json
    mediaType: application/json
```

`name` is a unique semantic key and `path` is a workspace-relative regular file.
The manifest may list zero or more stage-appropriate attachment entries. The
agentic result must leave `artifacts` empty; the staging manifest is a request for
the runner to lift bytes, not an `ArtifactPointer` source. For a task using
`artifactManifestFile`, the harness rejects a completion that self-reports any
artifact pointer instead of appending it to the runner-authored set.

The #1484 primitive validates the manifest and every path with the existing
containment and symlink rules, applies artifact size and count bounds, scrubs each
payload before digesting, and rejects duplicate names, missing files, directories,
unsupported media, or unsafe payloads. It then:

1. sorts entries by semantic `name`, lifts each payload, and authors its
   `ArtifactPointer`;
2. writes a normalized, runner-authored `artifact-set.json` containing each
   semantic name, its pointer, and its one-based `slot`;
3. returns the normalized index as result artifact 0 and the payload pointers in
   slot order as the remaining result artifacts.

The normalized index has this shape:

```yaml
schemaVersion: goobers.dev/stage-artifact-set/v1alpha1
entries:
  - name: reproduction.baseline
    slot: 1
    artifact: {path: artifacts/reproduce/<digest>, digest: "sha256:...", mediaType: application/json, size: 1234}
  - name: reproduction.bundle
    slot: 2
    artifact: {path: artifacts/reproduce/<digest>, digest: "sha256:...", mediaType: application/x-tar, size: 5678}
```

Downstream runtime handles consequently remain the existing
`<stage>.artifact[i]` names. `<stage>.artifact[0]` is always the normalized index;
an index entry with `slot: 2` must exactly match `<stage>.artifact[2]` by path and
digest. Consumers fail closed on a missing index, an unknown required semantic
name, a slot mismatch, or a digest mismatch. Semantic names used below are keys
inside that index, never promised `ContextPointer.Name` values. This convention
supports optional attachments without depending on their count or inventing a
new context naming contract.

Before the final evidence manifest, structured payloads refer to another payload
as `{producerStage, name}`, not by copying or predicting an `ArtifactPointer`.
`emit-evidence` resolves those semantic references through the normalized indices
and writes the corresponding runner-authored pointers into the canonical schema
in section 6.

## 4. Stage contracts

Every agentic stage declares a bounded timeout and infrastructure-retry policy.
`agent:model`, `repo:read`, and `repo:push` below are canonical capabilities, not
suggestions for new authority. Local writes inside a disposable worktree and writes
to the stage's result/artifact surface are execution affordances, not provider
capabilities. No stage receives issue, pull-request, merge, or arbitrary external
mutation authority.

### 4.1 `reproduce`

**Kind:** agentic, using a reproducer goober.

**Inputs**

- the immutable issue snapshot and triage note;
- target repository identity and pinned base revision;
- known environment facts, prior observations, and safety/resource budgets.

**Capabilities:** `repo:read`, `agent:model`.

**Outputs**

- artifact-set entry `reproduction.bundle`: a self-contained harness containing a
  manifest, launch/cleanup entrypoints, fixtures or workload definition, fixed
  seeds where applicable, and the machine-evaluable symptom oracle;
- artifact-set entry `reproduction.baseline`: a result recording the exact base
  revision, sanitized environment, invocation, attempts/load/duration, harness
  health signals, symptom observations, and semantic references to raw baseline
  evidence entries in the same artifact set;
- optional `reproduction.attachment.<id>` entries such as logs or fixture
  snapshots.

**Terminal outcomes**

- `success` only when the harness completed its workload and the oracle observed
  the claimed symptom under the declared baseline budget;
- `blocked` with `REPRODUCTION_NOT_ESTABLISHED` when the bounded, safe attempts did
  not reproduce the symptom or the required environment is unavailable; this
  escalates with all attempted-run evidence and does not advance;
- `failure` for a malformed/unsafe harness or an execution error after
  infrastructure retries are exhausted.

The `reproduction-established` gate reads `reproduce.artifact[0]`, resolves the
`reproduction.bundle` and `reproduction.baseline` entries and any baseline
evidence they reference, verifies every pointer/digest, and requires a healthy
harness run plus at least one oracle-confirmed baseline symptom before admitting
`instrument`.

### 4.2 `instrument`

**Kind:** agentic, using an investigator goober.

**Inputs**

- `reproduce.artifact[0]` and its indexed payload pointers, including required
  entries `reproduction.bundle` and `reproduction.baseline`;
- the immutable run inputs;
- the pinned base-revision worktree.

**Capabilities:** `repo:read`, `agent:model`.

**Outputs**

- artifact-set entry `diagnosis.report`: the hypotheses tested, controlled
  experiments, accepted causal chain, rejected alternatives, owning
  invariant/layer, confidence, and the justified fix altitude;
- artifact-set entry `diagnosis.evidence`: an index of supporting semantic
  attachment names in this artifact set;
- zero or more independently useful `diagnosis.attachment.<id>` entries containing
  dumps, traces, profiles, logs, metrics, or other diagnostic evidence.

**Terminal outcomes**

- `success` only when captured evidence supports a root cause and explains why the
  proposed fix belongs at the selected layer rather than merely suppressing the
  symptom;
- `blocked` with `ROOT_CAUSE_NOT_ESTABLISHED` when the budget is exhausted, the
  necessary instrumentation is unavailable, or competing hypotheses remain; no
  speculative fix is attempted;
- `failure` when capture is corrupt, unsafe, or internally inconsistent after
  infrastructure retries are exhausted.

The `cause-established` gate reads `instrument.artifact[0]` and requires
`diagnosis.evidence` to name at least one digest-verified attachment entry, plus an
accepted causal chain, rejected alternatives where applicable, and an explicit
fix-altitude rationale before admitting `implement-fix`.

### 4.3 `implement-fix`

**Kind:** agentic, using an implementer goober.

**Inputs**

- `reproduce.artifact[0]` and `instrument.artifact[0]`, plus their indexed payload
  pointers; required semantic entries are `reproduction.bundle`,
  `reproduction.baseline`, `diagnosis.report`, and `diagnosis.evidence`;
- the immutable run inputs and run branch based on the diagnosed revision.

**Capabilities:** `repo:push`, `agent:model`.

**Outputs**

- a committed product change on the run branch;
- scalar `fixRevision` naming the resulting commit;
- artifact-set entry `fix.report` mapping each material change to the diagnosed
  causal chain and documenting any retained regression test or production-safe
  instrumentation.

**Terminal outcomes**

- `success` only with a non-empty committed diff at the justified altitude and a
  fix report tied to the diagnosis;
- `blocked` when a named external dependency prevents a safe fix;
- non-retryable `failure` with `ISSUE_OVER_SCOPE` or `NEEDS_DECOMPOSITION` when the
  diagnosis proves that the claimed item cannot be one coherent change;
- `failure` for an implementation error after infrastructure retries are
  exhausted.

The `fix-produced` gate reads `implement-fix.artifact[0]`, verifies the commit
exists on the run branch, the diff is non-empty, and `fix.report` cites the
diagnosed invariant and evidence. It does not claim the fix works; that belongs
only to the next stage.

### 4.4 `validate-reproduction`

**Kind:** agentic, using a validation goober whose job is execution and evidence
capture, not source editing.

**Inputs**

- the exact `reproduce.artifact[0]` pointer and indexed payload pointers emitted
  by `reproduce`, including `reproduction.bundle` and `reproduction.baseline`;
- `instrument.artifact[0]`, `implement-fix.artifact[0]`, their indexed payload
  pointers, and scalar `fixRevision`; required semantic entries are
  `diagnosis.report` and `fix.report`;
- a fresh worktree at `fixRevision`.

**Capabilities:** `repo:read`, `agent:model`.

**Outputs**

- artifact-set entry `validation.result`: the reproduced baseline count/rate,
  bundle and oracle digests, fixed revision, sanitized environment, actual
  invocation, completed validation budget, harness health signals, post-fix
  symptom count/rate, and pass/fail decision;
- `validation.attachment.<id>` entries for raw validation logs/metrics and any
  post-fix diagnostics;
- optional `validation.supplemental.<id>` entries for ordinary unit, integration,
  or regression-test results, clearly marked as supplemental.

**Terminal outcomes**

- `success` only when the original bundle and oracle are unchanged, the harness
  health checks and workload complete, and the oracle observes zero instances of
  the original symptom across the declared validation budget;
- `failure` with `ORIGINAL_SYMPTOM_PERSISTS` when the original symptom appears even
  once, regardless of unit-test results;
- `blocked` when the baseline environment cannot be recreated safely or the
  declared validation budget cannot be completed;
- `failure` when the harness digest, oracle digest, target revision, or reported
  measurements do not verify.

For a probabilistic symptom, "gone" is necessarily bounded empirical evidence, not
a universal claim. The validation budget must be at least the baseline workload and
sample count, or carry a stronger predeclared statistical threshold. The result
states that bound and its confidence. It must also prove the harness did useful
work, so a no-op, early exit, disabled workload, or broken oracle cannot pass.

The `reproduction-cleared` gate reads `validate-reproduction.artifact[0]` and its
`validation.result` entry rather than an agent's prose. It verifies matching
digests, matching environment dimensions, the fixed revision, a completed budget,
healthy controls, and zero symptom observations. Only then may `emit-evidence`
run.

### 4.5 `emit-evidence`

**Kind:** agentic, using an evidence-packager goober.

**Inputs**

- `reproduce.artifact[0]`, `instrument.artifact[0]`,
  `implement-fix.artifact[0]`, and `validate-reproduction.artifact[0]`, plus all
  indexed payload pointers and required scalars emitted by those stages.

**Capabilities:** `agent:model`.

**Outputs**

- artifact-set entry `investigation.evidence`, containing
  `investigation-evidence.json`, the canonical manifest defined in section 6;
- a runner-authored pointer discoverable through `emit-evidence.artifact[0]` and
  suitable for downstream journal, portal, or provider surfaces;
- no duplicated raw payloads: the manifest indexes the already digested artifacts.

**Terminal outcomes**

- `success` when the manifest is schema-valid and every required or listed pointer
  resolves with its recorded digest;
- `failure` when a required field is absent, a pointer does not resolve, a digest
  differs, or the evidence cannot be durably written;
- no `blocked` outcome: unavailable optional evidence is omitted, while unavailable
  required evidence is a contract failure.

The `evidence-complete` gate resolves `investigation.evidence` through
`emit-evidence.artifact[0]` and performs schema and pointer validation. A pass
completes the run; a failure escalates with the already durable stage artifacts.

## 5. Harness and sandbox affordances

The investigation harness must make hard failures reproducible without turning the
agent into an unbounded host debugger.

- **Self-starting and self-cleaning:** the bundle starts every service, child
  process, fixture, and load generator it needs, records their identities, and
  tears down the complete process tree. It cannot depend on a daemon surviving
  between stage workspaces.
- **Controlled variation:** the manifest can pin or sweep seeds, concurrency,
  schedules, delays, fault injection, CPU/memory pressure, duration, and attempt
  count. The exact applied values are recorded in baseline and validation results.
- **Machine-evaluable oracle:** symptom detection is an exit status or structured
  result, not an agent's reading of an unbounded log. Harness-health controls are
  separate from the symptom oracle.
- **Bounded capture:** time, processes, CPU/memory pressure, disk, dump count, and
  artifact bytes are limited by the triage budget. Exceeding a bound is explicit,
  not silently truncated into a passing result.
- **Child-process scope:** debuggers, profilers, tracers, and dump collectors may
  inspect only processes started for the stage unless a separately reviewed
  execution policy grants more. Kernel tracing, elevated privileges, production
  attachment, or access to an external target is never inferred from `hard`;
  unavailable authority produces `blocked`.
- **Declared connectivity:** model access and any target-system endpoints are
  explicit execution requirements. A stage does not receive provider write access
  merely because its harness needs network access.
- **Disposable writable roots:** agent and harness writes stay in the stage
  worktree, stage telemetry directory, and declared artifact staging roots.
  OS-native sandboxing follows ADR 0001 where enabled and fails closed when the
  configured mechanism or a required profiling affordance is unavailable.
- **Redaction before durability:** dumps and traces can contain credentials,
  request bodies, paths, or user data. Every payload is scrubbed before digesting
  under SEC-041. Evidence that cannot be safely scrubbed is not emitted and the
  required/optional rules in section 6 determine whether the run can continue.

Harness execution and cleanup are workflow-level requirements for #1482; #1484
owns the artifact staging, redaction, and lifting boundary defined in section 3.1.
Neither issue broadens the canonical capability registry or bypasses the existing
stage sandbox.

## 6. Evidence-artifact schema

`investigation-evidence.json` is a small index over immutable
`ArtifactPointer` values, not a container for raw dumps. #1484 owns the encoded
schema and writer. Its logical shape is:

```yaml
schemaVersion: goobers.dev/investigation-evidence/v1alpha1
subject:
  item: <external issue reference>
  repository: owner/name
  baseRevision: <commit>
  fixRevision: <commit>
environment:
  platform: <sanitized platform identity>
  dimensions: {<comparison key>: <scalar value>}
  configDigest: sha256:<hex>
reproduction:
  harness: <ArtifactPointer>
  baseline: <ArtifactPointer>
  oracleDigest: sha256:<hex>
  symptom: <bounded description>
diagnosis:
  report: <ArtifactPointer>
  confidence: confirmed
  evidence: [<EvidenceRef>, ...]
fix:
  report: <ArtifactPointer>
validation:
  result: <ArtifactPointer>
  harnessDigest: sha256:<hex>
  oracleDigest: sha256:<hex>
  completedAttempts: <integer>
  symptomObservationsBefore: <integer>
  symptomObservationsAfter: 0
  passed: true
attachments: [<EvidenceRef>, ...]
```

An `EvidenceRef` contains:

```yaml
kind: dump | trace | profile | log | metrics | fixture | other
artifact: <ArtifactPointer>
producerStage: reproduce | instrument | implement-fix | validate-reproduction
description: <bounded text>
captureContext: {<comparison key>: <scalar value>}
```

Required fields are the subject revisions, sanitized comparison environment,
reproduction harness and baseline, oracle digest, diagnosis report with at least
one supporting evidence reference, fix report, and validation result. The
`attachments` array is optional and heterogeneous. No particular attachment kind
is mandatory: an investigation may emit a goroutine dump, a runtime trace, a CPU
profile, several kinds, or none when other causal evidence is sufficient. Listed
attachments must resolve and verify; absent optional kinds do not create empty
placeholder artifacts.

The schema reuses the stage contract's pointer fields (`path`, `digest`, optional
`mediaType` and `size`). It does not embed absolute paths, secret-bearing
environment values, or raw external content. The canonical manifest and each
payload are independently digest-addressed, allowing retention or presentation
layers to reason about them without changing stage handoffs.

## 7. Handoffs and gates

| Producer | Runtime handles and semantic entries | Admission condition |
|---|---|---|
| `reproduce` | `reproduce.artifact[0]` indexes `reproduction.bundle`, `reproduction.baseline`, and optional attachments. | Baseline harness is healthy and its oracle observed the symptom. |
| `instrument` | `instrument.artifact[0]` indexes `diagnosis.report`, `diagnosis.evidence`, and optional attachments. | Root cause, causal evidence, rejected alternatives, and fix altitude are recorded. |
| `implement-fix` | `implement-fix.artifact[0]` indexes `fix.report`; scalar `fixRevision` and the committed run branch travel separately. | The diff is non-empty and tied to the diagnosed invariant. |
| `validate-reproduction` | `validate-reproduction.artifact[0]` indexes `validation.result` and optional attachments. | The original digests match, the workload completed, and the original symptom count is zero. |
| `emit-evidence` | `emit-evidence.artifact[0]` indexes `investigation.evidence`. | Schema validates and every listed pointer resolves with its digest. |

The gates are deterministic contract checks, not additional investigation stages.
They never replace an artifact with evaluator prose. If a semantic assertion such
as the symptom predicate needs code, that code belongs in the immutable harness
and its result belongs in a structured artifact.

Every gate starts from the producer's slot-0 normalized index and verifies that
each referenced payload pointer is also present under the declared positional
runtime handle. It never assumes a semantic name was installed directly into
`ContextPointer.Name`.

## 8. Failure, retry, and escalation

- Dispatch and infrastructure failures use each stage's bounded `Task.Retry`
  policy. Each attempt is journaled separately. A business inability to reproduce,
  diagnose, or access a required environment is not retried by that policy.
- A `blocked` result ends the run `escalated` with its reason and partial artifact
  pointers. A recognized over-scope/decomposition result follows the existing
  non-retryable escalation disposition.
- A contract gate failure routes directly to `@escalate`. There is no automatic
  reproduce/instrument/fix loop and no validate-to-implement repass in this static
  design. A human may start a new pinned run with a revised budget or harness.
- An exhausted infrastructure failure ends `failed`; the terminal cause and every
  artifact successfully written before the failure remain in the append-only
  journal.
- If `validate-reproduction` observes the symptom, the fix is not accepted and
  `emit-evidence` is not run. Its failing validation result is itself durable
  evidence and is surfaced with the terminal run.
- A crash resumes from the last completed stage through normal journal replay.
  Stage-local processes are recreated from the bundle; they are never adopted as
  hidden recovered state.
- Stage 5 produces the canonical successful-investigation manifest. Earlier-stage
  normalized indices and payload artifacts are already durable individually, so
  failure before stage 5 does not discard the reproduction or diagnostics that
  were captured.

## 9. Follow-on ownership

| Issue | Owns | Explicitly does not own |
|---|---|---|
| #1482 | The static workflow definition, five goober roles/prompts, deterministic contract gates, harness execution/cleanup behavior, budgets, and workflow contract tests, built only after #1484's artifact-set primitive lands. | Automatic classification/routing, self-reported artifact pointers, or an ad hoc workflow-local artifact transport. |
| #1483 | Provisioning and documenting the `hard` triage label, the explicit manual start path, and mutual exclusion with ordinary implementation claims. | Inferring `hard` automatically or changing the five-stage graph. |
| #1484 | The versioned evidence schema; the `inputs.artifactManifestFile` agentic multi-file lifting primitive and normalized slot-0 index defined in section 3.1; runner-authored pointer validation; optional attachment-kind support; redaction-safe durable emission; and manifest/payload retention behavior. | Investigation reasoning, workflow routing, or requiring every diagnostic artifact kind. |

The required landing order is #1484's schema and artifact-set primitive, then
#1482's workflow against that shipped contract, then #1483 enabling the manual
route once a runnable target exists. #1482 is blocked by #1484; it must not emulate
multi-artifact lifting in prompts, rely on agent-authored pointers, or reinterpret
positional context names. Automatic routing requires a separate design and issue;
it is not a hidden last step of any of these three.

## 10. Non-goals

- Automatically classify issues as hard, heisenbugs, or load-dependent.
- Dynamically generate a workflow or add unbounded agentic replanning loops.
- Require a dump, trace, and profile from every investigation.
- Attach to production systems or arbitrary host processes by default.
- Replace the ordinary implementation, review, pull-request, or CI workflows.
- Treat a passing unit test as proof that the original end-to-end symptom is gone.
