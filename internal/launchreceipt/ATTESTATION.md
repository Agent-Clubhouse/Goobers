# Stage-attempt attestation activation (#4768)

PR3A is an internal, inert prerequisite. PR2A, PR2B, PR3A, and PR3B remain a
held draft stack until PR3B activates and verifies the complete path. The
PR3A deadcode exemptions must all be removed before that stack merges.

`Reader.Assemble` selects exactly one store through `RuntimeResolver`, validates
the complete controller binding, and reads at most the caller limit plus one
byte. Both receipt and resulting attestation must fit that limit, at most 8 KiB.
The resolver is trusted runtime composition: never construct it from an offline
`--path`, artifact path, directory name, or a receipt's claimed source. A missing
remote receipt cannot fall back to a local store. Unknown legacy authority uses
`UnknownStore` with no root. Reader methods never create or update files.

`Projection` is sealed in memory; its copied bytes and `Reference` are suitable
for durable transport. `StageAttemptAttestationArtifact` converts it to existing
`JournalArtifactOp.Data`. The existing live writer and `ProjectRun` retain exact
bytes, branch attribution, and an attempt-specific name. Serialization does not
retain the seal: any persisted body/reference is a locator, and readers must use
`Reader.Revalidate` against the selected source, exact binding, digest, and size.
The prepared facts carry their own allowlisted source, alongside the resolver's
source and fidelity. Local evidence is always unverified. Protected remote
evidence authenticates control-plane preparation only. Execution, completion,
resolved model/effort, grants, and enforcement remain explicitly unknown.

## PR3B integration points

- Local preparation already funnels through `RecordLocalInvocation` from
  `harness.Executor.recordPreparedLaunch` and `guardedDeterministic.Run`.
  `cmd/goobers/runnerwiring.go` supplies their `LocalRecorder`. Compose a durable
  publisher after the receipt write and before execution, with the exact
  `WithJournalStart` binding. That context currently carries no journal writer;
  install an explicit runtime-owned publisher capability for task, reviewer,
  retry, and parallel branch paths. Use the existing branch-attributed journal
  artifact write, and a stable attempt-specific idempotency key. On retry after
  a write/append interruption, validate existing bytes rather than minting a
  second receipt or silently skipping the reference.
- Remote preparation is accepted by `Store.Accept`, composed in
  `cmd/goobers/appendLaunchReceiptHandlerOption`, before
  `dispatcher.recordPreparedLaunch` returns and before pod creation. Keep the
  protected receipt write as the authority barrier. A publisher at this point
  alone is insufficient: non-live engine runs may have no run journal yet.
- Transport the canonical attestation bytes/reference from trusted runtime
  assembly into controller-owned activity metadata, independently of the
  model's `ResultEnvelope` and surrendered generic artifacts.
  `DispatchStageResult` and the corresponding local/reviewer prepared-activity
  results provide the metadata seam. Workflow consumers must append
  `StageAttemptAttestationArtifact` to the deterministic projection under a new
  `workflow.GetVersion` gate, covering DispatchOne, engine task/reviewer, local,
  retries, cancellation/failure, and resumed attempts. Preserve old histories.
  The private remote receipt boundary needs bounded trusted transport or a
  daemon-side activity; never let a worker-supplied root choose authority.
- Persist references during runtime publication, before relying on a read
  surface. A crashed/restarted daemon must recover publication from durable
  receipts and controller identity; non-live repair must recover byte-identical
  artifacts from recorded history. Define receipt-accepted/publication-pending
  behavior explicitly, including errors before an activity completes.
- Only after all publication paths exist should readservice/API/trace expose
  the contract. Reads only revalidate locators and do not repair/write state.
  `collectStageAttempts` can synthesize completion from `run.finished`; that
  synthesis must never upgrade attestation completion or execution fidelity.

Estimated remaining activation plumbing is roughly 200–350 production lines,
plus the separately planned readservice/API/trace work and tests. The precise
trusted remote transport and crash publication strategy require review before
expanding PR3B; exceeding its approved 2xL scope requires another explicit split.
