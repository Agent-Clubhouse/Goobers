# Design: Deep-investigation workflow

> **Status:** approved — accepted design (2026-07-25); runtime implementation is deferred.
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

> **The fix is not validated unless the runner seals the harness and symptom
> oracle that demonstrated the baseline defect, executes them against source
> snapshots attested to the base and fixed revisions, and no longer observes that
> defect under the declared validation budget.**

Unit and regression tests remain useful supplementary evidence, but cannot replace
that reproduction-gated end-to-end check.

This document defines the workflow contract. It does not add the workflow, labels,
runtime artifact types, or automatic routing. #1482 has a hard implementation
dependency on #1484: the workflow definition and its gates cannot land until
the manifest lifting, normalized index, pointer projection, and artifact-aware
resolver described in sections 3.1-3.2 are shipped as runtime contracts.

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
Neither pre-fix stage is attached to the managed run branch; its local edits and
commits remain in a stage-private detached repository. `implement-fix` is the first
stage that receives the run branch and changes product source there; it cannot edit
the journaled reproduction. `validate-reproduction` fails closed if the bundle or
symptom-oracle digest differs from the baseline.

All non-scalar data crosses stages as runner-authored `ArtifactPointer` values. The
implementation commit crosses through the run branch because only committed branch
state survives fresh worktrees. Transcripts and telemetry spans remain diagnostic;
a required handoff must be a declared artifact, not something a later stage scrapes
from a transcript.

### 3.1 Runner-authored artifact-set handoff

The shipped agentic harness can lift only one task-declared
`inputs.artifactFile`. It appends that runner-authored pointer after any
artifacts self-reported by the completion, so its only downstream address is the
positional `<stage>.artifact[i]`; it is `<stage>.artifact[0]` only when the
completion reports no artifacts. Agentic completion envelopes cannot be trusted
to mint their own paths or digests, and the current runner does not create
semantic context names such as `reproduction.bundle`. That single-file surface
is insufficient for a harness plus a variable set of dumps, traces, or profiles.

Therefore this design assigns **manifest-driven agentic artifact-set lifting** to
#1484 as a prerequisite for #1482, rather than letting #1482 create a
workflow-local transport. Each artifact-producing deep-investigation task declares
`artifactManifestFile` instead of the legacy `artifactFile`; declaring both is
invalid:

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
```

`name` is a unique semantic key and `path` is a workspace-relative regular file.
The manifest may list zero or more stage-appropriate attachment entries. The
agentic result must leave `artifacts` empty; the staging manifest is a request for
the runner to lift bytes, not an `ArtifactPointer` source. For a task using
`artifactManifestFile`, the harness rejects a completion that self-reports any
artifact pointer rather than merging untrusted pointers with the runner-authored
set. The staging manifest itself is control input and is not lifted or exposed
downstream.

The #1484 primitive validates the manifest and every path with the existing
containment and symlink rules, applies artifact size and count bounds, scrubs each
payload before digesting, and rejects duplicate names, missing files, directories,
unsupported media, or unsafe payloads. Validation covers the complete set before
publication; a failure publishes no `ResultEnvelope.Artifacts`. It then:

1. sorts entries by semantic `name`, lifts each payload, and authors its
   `ArtifactPointer`;
2. writes a normalized, runner-authored `artifact-set.json` containing each
   semantic name, its pointer, and its one-based `slot`;
3. returns exactly the normalized index as result artifact 0 followed by the
   payload pointers in slot order.

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
slots are unique and contiguous from 1 through the number of sorted payloads; and
an index entry with `slot: 2` must exactly match `<stage>.artifact[2]` in every
`ArtifactPointer` field. The V0 runner already carries every artifact from every
completed producer into later task and agentic-gate invocations, preserving result
order as `<stage>.artifact[i]`, and reconstructs the same order from the journal on
resume. Section 3.2 assigns the equivalent projection for automated gates to
#1484.

The static workflow declares no per-attachment data-flow edge and does not need to
predict an optional attachment count: a consumer reads slot 0, looks up a semantic
key, and selects the positional context pointer named by its slot. Consumers fail
closed on a missing or malformed index, a duplicate or out-of-range slot, an
unknown required semantic name, or any pointer mismatch. Semantic names used below
are keys inside that index, never promised `ContextPointer.Name` values. This
convention supports optional attachments without inventing a new context naming
contract.

Before the final evidence manifest, structured payloads refer to another payload
as `{producerStage, name}`, not by copying or predicting an `ArtifactPointer`.
`emit-evidence` resolves those semantic references through the normalized indices
and writes the corresponding runner-authored pointers into the canonical schema
in section 6.

### 3.2 Artifact-aware deterministic gates

The shipped automated-gate evaluator is a pure function over the subject result's
scalar status and outputs. It receives no context pointers or worktree and cannot
resolve the artifact sets required by this workflow. #1484 must therefore add an
artifact-aware automated-check seam as part of the artifact-set primitive, before
#1482 defines these gates:

1. automated-gate dispatch includes the accumulated upstream
   `ContextPointer` values in its invocation envelope, using the same positional
   names and order supplied to later agentic stages;
2. the evaluator receives a read-only, current-run artifact resolver that applies
   the existing containment, size, and digest checks directly against the journal;
   it receives no worktree or general filesystem access;
3. a reusable artifact-set resolver loads `<producer>.artifact[0]`, validates the
   normalized index, verifies every indexed pointer against the corresponding
   positional context pointer, and returns a semantic-name lookup over only those
   verified payloads;
4. artifact-aware checks form a separate registered check type, leaving the
   existing scalar `CheckFunc` registry pure and backward compatible.

#1482 registers the five workflow-specific `pass`/`fail` checks on that seam:
`reproduction-established`, `cause-established`, `fix-produced`,
`reproduction-cleared`, and `evidence-complete`. Each parses only the bounded
structured payloads named in its stage contract and applies the admission
condition stated below. A malformed set, absent required name, pointer mismatch,
schema violation, or false admission predicate returns `fail` and follows the
gate's explicit escalation branch. A transient journal read failure is an
evaluator error governed by the gate's infrastructure retry policy.

Artifact bytes are never flattened into scalar `inputs` or `outputs`, and the
runner does not attest an agent-authored boolean in place of checking the evidence.
This preserves deterministic gates and the envelope contract while making every
gate a concrete consumer of the same runner-authored positional pointers as later
stages.

The artifact resolver does not by itself attest repository state. #1482 also owns
a narrow read-only run-revision verifier for `fix-produced`,
`reproduction-cleared`, and `evidence-complete`. The runner binds it to the pinned
base revision and active run branch; it exposes only the branch head, commit
reachability on that branch, and the SHA-256 digest and emptiness of the canonical
`base...HEAD` diff. It uses the runner's managed repository service and does not
give the gate a worktree or arbitrary filesystem access.

`fix-produced` requires the scalar `fixRevision` and the revision and diff digest
recorded in `fix.report` to match that verifier, including a non-empty diff.
`reproduction-cleared` compares `validation.result` and the already verified
`fix.report` to the same branch head and diff digest. `evidence-complete` repeats
that comparison after `emit-evidence` and couples a matching attestation to the
terminal success transition. The agent may propose revision values, but no gate
trusts them without the runner-owned comparison.

### 3.3 Pre-fix branch boundary

The absence of `repo:push` does not stop an agent from committing to a locally
attached branch. #1482 therefore owns a workflow-specific workspace and ref policy
that prevents `reproduce` and `instrument` from advancing or contributing commits
to the run branch:

1. At admission, the runner resolves and records `baseRevision` and reserves the
   run-branch name. Until `cause-established` passes, the managed run-branch ref
   must either be absent or point exactly to `baseRevision`, with an empty canonical
   `base...HEAD` diff. This supports runners that create the ref lazily and runners
   whose generic setup creates it at the base, without trusting either state
   implicitly.
2. The runner provisions `reproduce` and `instrument` from the exact base revision
   as stage-private, detached repository copies. Each copy has private Git metadata,
   object storage, and refs; the managed repository path, common Git directory, and
   run-branch ref are not mounted or otherwise addressable from the stage sandbox.
   The stage may edit files and create commits for exploratory probes, but those
   commits can advance only private detached state and are discarded with the
   workspace. Only declared artifacts cross the stage boundary.
3. Immediately before each pre-fix adapter starts, and after its complete process
   tree and runner finalizers have exited but before its result and artifact set are
   committed, the runner takes the run-branch ref lock and verifies the allowed
   absent-or-exact-base state. A mismatch fails closed with
   `PRE_FIX_BRANCH_DRIFT`; the runner neither resets the ref nor incorporates its
   diff, and that integrity failure is not retried. An ordinary infrastructure
   retry unrelated to branch drift receives a new detached copy and repeats both
   checks.
4. After `cause-established` passes and immediately before `implement-fix`, the
   runner repeats that verification under the ref lock, atomically creates the run
   branch at `baseRevision` when it is absent (or accepts the existing exact-base
   ref), and records the initialized base in workflow state. Only then may it
   provision a writable worktree attached to that branch for `implement-fix`.

The inaccessible managed ref is the preventive boundary; the locked before/after
checks detect runner or external drift. Together they ensure the later
`base...HEAD` digest can contain only changes made after the implementation boundary,
not exploratory instrumentation.

### 3.4 Sealed revision execution

Removing `repo:push` credentials is not a repository-integrity boundary. An agent
can still edit tracked files, change a staged harness, create local commits, or
restore modified bytes before an after-the-fact verifier runs. Branch metadata
therefore cannot prove which bytes a harness executed.

#1482 must add a runner-owned **sealed revision executor** for the authoritative
baseline and validation runs. This is a stage finalizer, not an agent tool or a
sixth workflow stage. It runs after the agent adapter has exited and the runner
has terminated the adapter's complete process tree, but before the stage result
and artifact set are committed:

1. For `reproduce`, the runner freezes the staged `reproduction.bundle` into a
   runner-private, digest-verified input. For `validate-reproduction`, it resolves
   the already journaled `reproduction.bundle` pointer. The agent cannot replace
   either frozen input during execution.
2. The runner resolves the requested commit from its managed Git object store and
   materializes a detached source snapshot. The baseline target is the pinned base
   revision. The validation target is `fixRevision`, after the run-revision
   verifier confirms that it is still the active run-branch head and that its
   canonical diff digest matches the accepted `fix.report`.
3. The runner records the commit and tree object IDs and a canonical SHA-256
   source-snapshot digest over every materialized path, file mode, and byte
   sequence. The executor exposes the source snapshot, frozen harness, immutable
   run inputs, and fixtures read-only. Only declared runtime scratch, build-cache,
   capture, and artifact-staging roots are writable; the process working
   directory is one of those scratch roots, never the source tree.
4. The runner launches the bundle's pinned entrypoint under an OS enforcement
   policy that denies writes to the source snapshot and bundle. The agent and its
   descendants are not concurrent with this run. If the platform cannot enforce
   those mounts or access rules, or the workload requires in-place source
   generation, the finalizer fails closed rather than falling back to a writable
   checkout. Pre- and post-execution digests are also compared as defense in
   depth, but write denial is the control that prevents modify-then-restore.
5. The runner captures the actual argv, sanitized environment dimensions,
   policy version, budget consumed, harness-health controls, oracle observations,
   source and harness digests, exit status, and semantic references to raw
   captures in a runner-authored execution record. Agent-authored summaries may
   add bounded context, but cannot override those fields.

For `reproduce`, `reproduction.baseline` is the sealed executor's record. For
`validate-reproduction`, `validation.result` is its record. Those names are
reserved: an agent staging manifest that supplies either name is rejected. The
finalizer adds the reserved record and its captured attachments to the candidate
artifact set, and #1484's publisher validates and lifts the complete set
atomically. No partial pointers are published if execution or final publication
fails.

An adapter must return provisional `success` before its finalizer runs. The
finalizer then derives the committed stage status and domain error code from the
sealed execution: baseline not established becomes
`blocked`/`REPRODUCTION_NOT_ESTABLISHED`, a post-fix symptom becomes
`failure`/`ORIGINAL_SYMPTOM_PERSISTS`, and only the admission conditions in
sections 4.1 and 4.4 become `success`. An adapter `blocked` or `failure` remains
terminal and cannot be upgraded by the finalizer.

An agent may run exploratory commands in its disposable worktree while developing
the harness or preparing validation context, but those runs are never admissible
evidence. Only the finalizer can author the baseline and validation records
consumed by the gates.

Once `fix-produced` passes, the runner persists the accepted `(baseRevision,
fixRevision, diffDigest)` tuple in workflow state. Later stages receive no
writable checkout of the active run branch: validation receives only the sealed,
detached snapshot, and `emit-evidence` uses a scratch workspace. The verifier
checks the tuple immediately before and after sealed validation and again after
evidence emission. The final check and successful run transition occur under the
managed repository's run-branch ref lock; a mismatch or concurrent ref update
fails `evidence-complete`. This repeated runner attestation, rather than missing
credentials or agent-reported hashes, binds the successful run to the reviewed
revision.

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
- the runner-owned, stage-private detached base workspace from section 3.3;
- known environment facts, prior observations, and safety/resource budgets.

**Capabilities:** `repo:read`, `agent:model`.

**Outputs**

- artifact-set entry `reproduction.bundle`: a self-contained harness containing a
  manifest, launch/cleanup entrypoints, fixtures or workload definition, fixed
  seeds where applicable, and the machine-evaluable symptom oracle;
- runner-authored artifact-set entry `reproduction.baseline`: the sealed-execution
  record from section 3.4, including the exact base source-snapshot digest,
  sanitized environment, invocation, attempts/load/duration, harness health
  signals, symptom observations, and semantic references to raw baseline evidence
  entries in the same artifact set;
- optional `reproduction.attachment.<id>` entries such as logs or fixture
  snapshots.

**Terminal outcomes**

- `success` only when the sealed finalizer completed the harness workload against
  the pinned base snapshot and the oracle observed the claimed symptom under the
  declared baseline budget;
- `blocked` with `REPRODUCTION_NOT_ESTABLISHED` when the bounded, safe attempts did
  not reproduce the symptom or the required environment is unavailable; this
  escalates with all attempted-run evidence and does not advance;
- `failure` for a malformed/unsafe harness or an execution error after
  infrastructure retries are exhausted.

The `reproduction-established` gate reads `reproduce.artifact[0]`, resolves the
`reproduction.bundle` and `reproduction.baseline` entries and any baseline
evidence they reference, verifies every pointer/digest, and requires the
runner-authored source, bundle, oracle, and execution attestations plus a healthy
harness run and at least one oracle-confirmed baseline symptom before admitting
`instrument`.

### 4.2 `instrument`

**Kind:** agentic, using an investigator goober.

**Inputs**

- `reproduce.artifact[0]` and its indexed payload pointers, including required
  entries `reproduction.bundle` and `reproduction.baseline`;
- the immutable run inputs;
- a new runner-owned, stage-private detached base workspace from section 3.3.

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
- the immutable run inputs and the run branch initialized and re-attested at
  `baseRevision` under section 3.3's ref lock.

**Capabilities:** `repo:push`, `agent:model`.

**Outputs**

- a committed product change on the run branch;
- scalar `fixRevision` naming the resulting commit;
- artifact-set entry `fix.report` recording `fixRevision`, the canonical
  `base...HEAD` diff digest, mapping each material change to the diagnosed causal
  chain, and documenting any retained regression test or production-safe
  instrumentation.

**Terminal outcomes**

- `success` only with a non-empty committed diff at the justified altitude and a
  fix report tied to the diagnosis;
- `blocked` when a named external dependency prevents a safe fix;
- non-retryable `failure` with `ISSUE_OVER_SCOPE` or `NEEDS_DECOMPOSITION` when the
  diagnosis proves that the claimed item cannot be one coherent change;
- `failure` for an implementation error after infrastructure retries are
  exhausted.

The `fix-produced` gate reads `implement-fix.artifact[0]`, uses the run-revision
verifier from section 3.2 to attest the branch head and non-empty diff, and
requires the scalar and `fix.report` revision/digest to match it.
`fix.report` must also cite the diagnosed invariant and evidence. The gate does
not claim the fix works; that belongs only to the next stage.

### 4.4 `validate-reproduction`

**Kind:** agentic, using a validation goober whose job is execution and evidence
capture, not source editing.

**Inputs**

- the exact `reproduce.artifact[0]` pointer and indexed payload pointers emitted
  by `reproduce`, including `reproduction.bundle` and `reproduction.baseline`;
- `instrument.artifact[0]`, `implement-fix.artifact[0]`, their indexed payload
  pointers, and scalar `fixRevision`; required semantic entries are
  `diagnosis.report` and `fix.report`;
- the runner-owned sealed, detached source snapshot at `fixRevision`; the active
  run branch and the validation agent's exploratory worktree are not mounted into
  the authoritative execution.

**Capabilities:** `repo:read`, `agent:model`.

**Outputs**

- runner-authored artifact-set entry `validation.result`: the sealed-execution
  record containing the reproduced baseline count/rate, bundle and oracle
  digests, fixed revision, source-snapshot and canonical diff digests, sanitized
  environment, actual invocation, completed validation budget, harness health
  signals, post-fix symptom count/rate, and pass/fail decision;
- `validation.attachment.<id>` entries for raw validation logs/metrics and any
  post-fix diagnostics;
- optional `validation.supplemental.<id>` entries for ordinary unit, integration,
  or regression-test results, clearly marked as supplemental.

**Terminal outcomes**

- `success` only when the sealed finalizer ran the original bundle and oracle
  unchanged against the attested `fixRevision`, the harness health checks and
  workload complete, and the oracle observes zero instances of the original
  symptom across the declared validation budget;
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
runner-authored `validation.result` entry rather than an agent's prose. It
verifies matching bundle, oracle, and environment digests; source-snapshot
digests bound respectively to the pinned base and accepted fix trees; a fixed
revision and diff digest that match both `fix.report` and the run-revision
verifier before and after execution; a completed budget; healthy controls; and
zero symptom observations. Only then may `emit-evidence` run.

### 4.5 `emit-evidence`

**Kind:** agentic, using an evidence-packager goober.

**Workspace:** scratch. This stage receives artifact pointers and scalars but no
repository checkout or managed-repository path.

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
`emit-evidence.artifact[0]` and performs schema and pointer validation. It then
re-runs the run-revision verifier under the run-branch ref lock and requires the
head and canonical diff digest to match the accepted fix tuple and
`validation.result`. A pass completes the run in that locked transition; a
failure escalates with the already durable stage artifacts.

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
  Pre-fix worktrees use section 3.3's private detached repositories and cannot
  address managed refs.
  OS-native sandboxing follows ADR 0001 where enabled and fails closed when the
  configured mechanism or a required profiling affordance is unavailable. The
  authoritative baseline and validation finalizers use section 3.4's stricter
  sealed execution roots; exploratory worktree execution cannot substitute for
  them.
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
  sourceSnapshotDigest: sha256:<hex>
  oracleDigest: sha256:<hex>
  symptom: <bounded description>
diagnosis:
  report: <ArtifactPointer>
  confidence: confirmed
  evidence: [<EvidenceRef>, ...]
fix:
  report: <ArtifactPointer>
  diffDigest: sha256:<hex>
validation:
  result: <ArtifactPointer>
  sourceSnapshotDigest: sha256:<hex>
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
reproduction harness and runner-authored baseline, baseline and validation
source-snapshot digests, oracle digest, diagnosis report with at least one
supporting evidence reference, fix report and canonical diff digest, and the
runner-authored validation result. The `attachments` array is optional and
heterogeneous. No particular attachment kind is mandatory: an investigation may
emit a goroutine dump, a runtime trace, a CPU profile, several kinds, or none when
other causal evidence is sufficient. Listed attachments must resolve and verify;
absent optional kinds do not create empty placeholder artifacts.

The schema reuses the stage contract's pointer fields (`path`, `digest`, optional
`mediaType` and `size`). It does not embed absolute paths, secret-bearing
environment values, or raw external content. The canonical manifest and each
payload are independently digest-addressed, allowing retention or presentation
layers to reason about them without changing stage handoffs.

## 7. Handoffs and gates

| Producer | Runtime handles and semantic entries | Admission condition |
|---|---|---|
| `reproduce` | `reproduce.artifact[0]` indexes `reproduction.bundle`, runner-authored `reproduction.baseline`, and optional attachments. | The sealed base snapshot and bundle attestations match; the baseline harness is healthy and its oracle observed the symptom. |
| `instrument` | `instrument.artifact[0]` indexes `diagnosis.report`, `diagnosis.evidence`, and optional attachments. | Root cause, causal evidence, rejected alternatives, and fix altitude are recorded. |
| `implement-fix` | `implement-fix.artifact[0]` indexes `fix.report`; scalar `fixRevision` and the committed run branch travel separately. | The diff is non-empty and tied to the diagnosed invariant. |
| `validate-reproduction` | `validate-reproduction.artifact[0]` indexes runner-authored `validation.result` and optional attachments. | The sealed fixed snapshot, accepted fix tuple, and original digests match; the workload completed and the original symptom count is zero. |
| `emit-evidence` | `emit-evidence.artifact[0]` indexes `investigation.evidence`. | Schema and pointers validate, then the run branch is re-attested to the accepted fix tuple under the final ref lock. |

The gates are deterministic contract checks, not additional investigation stages.
They never replace an artifact with evaluator prose. If a semantic assertion such
as the symptom predicate needs code, that code belongs in the immutable harness
and its result belongs in a structured artifact.

Every gate starts from the producer's slot-0 normalized index and verifies that
each referenced payload pointer is also present under the declared positional
runtime handle through section 3.2's read-only artifact-set resolver. Optional
payload handles are discovered from the index delivered with the producer's
complete artifact list, not enumerated in the static workflow definition. A gate
never assumes a semantic name was installed directly into `ContextPointer.Name`.

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
- `PRE_FIX_BRANCH_DRIFT` is a non-retryable integrity failure. It terminates before
  `implement-fix`, preserves any artifacts committed by earlier completed stages,
  and reports the expected base and observed ref state; the runner never resets or
  adopts the unexpected branch.
- An exhausted infrastructure failure ends `failed`; the terminal cause and every
  artifact successfully written before the failure remain in the append-only
  journal.
- If `validate-reproduction` observes the symptom, the fix is not accepted and
  `emit-evidence` is not run. Its failing validation result is itself durable
  evidence and is surfaced with the terminal run.
- A crash resumes from the last completed stage through normal journal replay.
  Stage-local processes are recreated from the bundle; they are never adopted as
  hidden recovered state. A partially completed sealed finalizer publishes no
  artifact set and is rerun as a new attempt against the same resolved commit and
  frozen bundle.
- Stage 5 produces the canonical successful-investigation manifest. Earlier-stage
  normalized indices and payload artifacts are already durable individually, so
  failure before stage 5 does not discard the reproduction or diagnostics that
  were captured.

## 9. Follow-on ownership

| Issue | Owns | Explicitly does not own |
|---|---|---|
| #1482 | The static workflow definition, five goober roles/prompts, five workflow-specific artifact-aware checks built on #1484's resolver, the private detached pre-fix workspace policy and locked branch initializer from section 3.3, the narrow run-revision verifier used by the repository-sensitive checks, the sealed revision executor and runner-owned reproduce/validate finalizers from section 3.4, the final ref-locked attestation, harness execution/cleanup behavior, budgets, and workflow contract tests, built only after #1484's artifact-set primitive lands. | Automatic classification/routing, exposing a managed ref to pre-fix stages, treating a writable worktree or missing `repo:push` as revision attestation, self-reported execution records or artifact pointers, or an ad hoc workflow-local artifact transport. |
| #1483 | Provisioning and documenting the `hard` triage label, the explicit manual start path, and mutual exclusion with ordinary implementation claims. | Inferring `hard` automatically or changing the five-stage graph. |
| #1484 | The versioned evidence schema; the `inputs.artifactManifestFile` agentic multi-file lifting primitive and normalized slot-0 index defined in section 3.1; automated-gate pointer projection, read-only resolver, and artifact-aware check seam from section 3.2; runner-authored pointer validation; optional attachment-kind support; redaction-safe durable emission; and manifest/payload retention behavior. | Investigation reasoning, workflow-specific admission predicates, routing, or requiring every diagnostic artifact kind. |

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
