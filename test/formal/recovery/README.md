# Bounded terminal-journal recovery model (#8314)

## Exactly one semantic property

`TerminalReplayStable`: after ordinary recovery reconstructs a terminal event
log, its recovered phase equals that log's terminal outcome, never `running`
or a different terminal outcome. The model checks one append attempt for one
local-runner run. It does **not** model all Goobers recovery or cancellation.
`TypeOK` additionally checks the model's variable domains.

The boundary is the complete `run.finished` record in `events.jsonl`, not
`state.json`. The latter is a derived checkpoint. A terminal record successfully
synced before a crash survives; a complete but unsynced record may also survive.
Recovery accepts a surviving complete record, discards a torn final record, and
reconstructs phase from the log before healing the checkpoint.

## Implementation grounding and trace witnesses

Baseline inspected: `0ff1880d2` (PR #8316 / #8312).

| Model action/path | Implementation and executable witness |
|---|---|
| `BeginAppend`, `CompleteWrite`, `SyncTerminal` | `internal/journal/run.go`: `Append` -> `appendPrepared` -> `append` -> `appendEvent`; the event write/sync precedes lifecycle update and checkpoint. `internal/runner/run.go`: `finishTakeoverWithDisposition` appends `run.finished` after terminal preparation. |
| `SyncTerminal -> Crash -> Recover -> HealCheckpoint` | `internal/journal/reader.go`: `Recover` reads the log under the run lock, uses `reconstructPhase`, and heals a stale terminal checkpoint. `TestRecoverHealsStaleTerminalCheckpoint` and new `TestTerminalRecoveryModelTraces/synced-stale-checkpoint` (nested under each terminal phase) recreate those bytes, then recover twice. |
| `SyncTerminal -> Checkpoint -> Crash -> Recover` | New `TestTerminalRecoveryModelTraces/synced-current-checkpoint` retains the current checkpoint; phase and the normative terminal prefix remain unchanged across two recoveries. |
| `BeginAppend -> Crash -> Recover` | New `TestTerminalRecoveryModelTraces/partial-before-sync` writes a partial terminal line, then checks that repair preserves the running prefix rather than inventing a terminal outcome. |
| `Recover -> HealCheckpoint -> ReturnResume`, repeated restart | `internal/runner/resume.go`: `Resume` -> `resumeOwned` -> `resumeTerminalPhase` reads `Reader.Phase`, returns the existing outcome without walking. New `TestRunnerTerminalRecoveryModelTraces` checks all four phases with in-process executor fakes, zero dispatches, and unchanged `ConformanceView` across two resumes. Existing `TestRunnerResumeTrustsJournalOverStaleTerminalCheckpoint` also checks no repeated gate evaluation/terminal event. |

Journal tests simulate the post-crash files by completing a normal append and
overwriting the checkpoint (or appending a partial line). They do not inject
power loss inside a syscall. They compare the normative event prefix using the
existing `ConformanceView` contract; see
`TestConformanceViewCapturesFullNormativeFieldSet`. The partial-record case
allows the implementation's extra `repaired` diagnostic, which is abstracted
away by the model.

Also inspected `internal/engine/journal.go` and
`internal/engine/lifecycle_replay_test.go`: Temporal reconstructs journal
projections from recorded workflow history. Its workflow-task replay is a
different recovery boundary. This model makes no claim about engine history,
activity retries, journal emission receipts, or distributed workflow workers.

## Assumptions, crash locations, and finite bounds

- One run, one terminal append attempt; the outcome is initially chosen from
  `completed`, `failed`, `aborted`, `escalated` and never changed by an author.
  At most two crashes/restarts. After recovery this small model does not
  continue a nonterminal workflow or start another append.
- One serialized writer/recoverer. The journal's mutex and per-run file lock
  are assumed effective, not verified here. No concurrent reader/writer,
  watchdog, operator intervention, child, branch, or provider is represented.
- Crash can occur before writing, after partial writing, after a complete
  write but before sync, after sync but before checkpoint, after checkpoint,
  during recovery before healing, after healing, and after returning.
  Recovery's read/reconstruct and checkpoint replacement are separate atomic
  model actions. No crash inside their atomic abstract operations is modeled.
- A complete unsynced line nondeterministically survives or is lost/torn.
  An incomplete line never becomes a valid terminal event. Successful sync
  persists the complete line; checkpoints replace atomically with old or new
  contents. No media corruption, lost previously synced bytes, malformed
  interior records, filesystem failure, or fsync-disable setting is modeled.
- **Provider idempotency is not assumed or modeled.** There are no external
  effects, keys, or delivery claims. This is not an exactly-once proof, nor a
  proof that finalization callbacks run once. It does not check cancellation
  admission or protected mutations.
- Ordinary crash recovery only: explicit `run.resumed`, gate overrides, and
  stage-rerun requests can legitimately reopen a terminal run and are excluded.
  Existing `TestRunResumedReopensTerminalPhaseAndRecoversTarget` guards that
  deliberate non-monotonic behavior separately.
- No fairness assumptions (`WF`/`SF`) or liveness claim. Stuttering is allowed.
  Deadlock checking is intentionally disabled: quiescent returns and an
  exhausted crash budget are legitimate finite-model ends.

## Reproduce with real TLC

Use the public [TLA+ v1.8.0 release](https://github.com/tlaplus/tlaplus/releases/tag/v1.8.0)
`tla2tools.jar`, SHA-256:

```text
e20447644bfabd29d6bd0fc2c0eb1473fb301c1a0e4dfe1961938ce5a191edbf
```

The recorded run used TLC's banner `TLC2 Version 2026.11.03.180953 (rev: 309e489)`
and Eclipse Temurin Windows x64 JRE `21.0.13+9-LTS`. The public
[Temurin 21.0.13+9 JRE archive](https://github.com/adoptium/temurin21-binaries/releases/tag/jdk-21.0.13%2B9)
was verified against release metadata SHA-256
`2332c0b4cff6bed722da3567c91d5e47568bcba863fa4945d0a8d6fc76158628`.
Neither tool is vendored into source; no download occurs during a check.
Set `JAVA` to that Java executable and `TLC_JAR` to the verified jar.

PowerShell, starting at the repository root:

```powershell
Set-Location test\formal\recovery
New-Item -ItemType Directory -Force .tlc-work | Out-Null
$common = @('-Djava.io.tmpdir=.tlc-work', '-XX:+UseParallelGC',
    '-cp', $env:TLC_JAR, 'tlc2.TLC', '-workers', '1',
    '-seed', '1', '-fp', '0', '-noGenerateSpecTE')
& $env:JAVA @common -metadir .tlc-work\good `
    -config TerminalRecovery.cfg `
    -dumpTrace json results\counterexample.json TerminalRecovery.tla
if ($LASTEXITCODE -ne 0) { throw 'Good model failed' }
& $env:JAVA @common -metadir .tlc-work\bad `
    -config TerminalRecoveryBad.cfg `
    -dumpTrace json results\counterexample.json TerminalRecovery.tla
if ($LASTEXITCODE -ne 12) { throw 'Expected invariant counterexample' }
Remove-Item -Recurse -Force .tlc-work
```

These are direct `java -cp tla2tools.jar tlc2.TLC` invocations, not a bespoke
checker. The local Java extraction directory prevents TLC from using OS
temporary directories. `-fp 0`, seed 1, and one worker fix exploration order.
Capture stdout/stderr if refreshing the logs.

From the repository root, implementation witnesses:

```powershell
$env:GOTOOLCHAIN = 'go1.26.10'
go test ./internal/runner ./internal/journal -run '^Test(TerminalRecoveryModelTraces|RunnerTerminalRecoveryModelTraces|RecoverHealsStaleTerminalCheckpoint|ReaderPhaseReconstructsFromLogNotCheckpoint|RunResumedReopensTerminalPhaseAndRecoversTarget|RunnerResumeTrustsJournalOverStaleTerminalCheckpoint|RunnerResumeReplaysBlockedFinishAsEscalated|ConformanceViewCapturesFullNormativeFieldSet)$' -count=1
go vet ./internal/journal ./internal/runner
```

## Actual measured results and negative control

Recorded 2026-11-03 on Windows x64:

| Configuration | TLC result | Generated / distinct / queued | Search depth |
|---|---|---|---|
| `TerminalRecovery.cfg`, `TrustCheckpoint = FALSE` | Exhausted graph, no invariant violation, exit 0 | 140 / 104 / 0 | 9 |
| `TerminalRecoveryBad.cfg`, `TrustCheckpoint = TRUE` | `TerminalReplayStable` violated, exit 12; stopped early | 51 / 42 / 15 | 5 |

Path-redacted actual stdout/stderr: [good](results/good.log),
[mutation](results/bad.log). These are not raw byte-for-byte evidence:
only machine-specific absolute paths were replaced by `<MODEL_DIR>`,
`<TLC_MODULE_DIR>`, and `<TLC_JAR>` placeholders. Counts, errors, actions,
source locations, timestamps, and all other output are retained.
The [native TLC JSON counterexample](results/counterexample.json) retains all
five states and action/source locations; it contained no machine-specific
absolute paths and is unchanged. Its shortest path is:

```text
Init(completed) -> BeginAppend -> CompleteWrite
  -> Crash(complete unsynced record survives) -> Recover
logPhase = completed, checkpoint = running, replayPhase = running  [VIOLATION]
```

The post-crash bytes are equivalent to the synced/stale-checkpoint Go witness.
The longer `SyncTerminal -> Crash -> Recover` path is also admitted by the
model. Only the model parameter changes for the negative control: no production
guard, permission, trust boundary, sandbox, or implementation is weakened.

The command above passed both Go packages; the new tests exercise 12 journal
cases and four runner cases, each with two recoveries. The package vet command
also passed.

## Scope of evidence, not implementation refinement

This is finite-state model checking under the stated assumptions, with TLC's
64-bit state fingerprints (the good run reports an optimistic collision
estimate of `2.0E-16`), not a theorem about unbounded executions. The chosen Go
traces provide concrete conformance witnesses, **not a proof that the Go
implementation refines the TLA+ specification**, a disk durability experiment,
or verification of the full daemon.

For #8312/#8313 integration, pin this reviewed specification/configuration,
tool hash, bounds, and assumptions as trusted artifacts. The independent
checker's finite-graph candidate format is a separate adapter boundary; this
directory does not claim that adapter has verified this model or fabricate a
`VerificationReport`. Publication requires recording the real subject,
specification, context, and checker evidence artifacts first. Any successful
`proven` outcome must be explicitly scoped to this finite model, not to the Go
implementation, arbitrary application code, or provider effects.
