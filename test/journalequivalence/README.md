# Local and cloud journal equivalence

`TestCloudJournalEquivalentToLocal` runs the same DSL 3.0 corpus and scripted
executors as `TestLocalJournalEquivalenceBaseline`, three times per fixture.
The cloud leg executes the engine with a PostgreSQL journal and explicitly
asserts `current_user = goobers_controller`. The local leg uses the real runner
and file journal. Both use one canonicalizer for complete event bytes, artifact
digests, and the A2 read-model projection; the checked-in local SHA-256 values
remain the reference.

| Fixture | Contract |
| --- | --- |
| retry-artifact-inputs-and-gates | Exact equality, including recorder receipts, reviewer redaction evidence, and artifact-before-review ordering |
| human-gate | Exact equality through pause and approval |
| infrastructure-retry | Exact equality, including domain error classification and causes |
| stage-boundary-cancellation | Exact equality at worker drain; replacement worker continues without executing the first stage twice |
| gate-fail-rerun | Known divergence, pending #7899; exact equality before intervention, then acknowledged Temporal update followed by terminal journal refusal |
| terminal-resume | Known divergence, pending #7899; exact equality before intervention, then acknowledged Temporal update followed by terminal journal refusal |

The two pending fixtures are assertions of the exact known failure, not blanket
skips or claims of full equivalence. They compare their entire pre-intervention
journal and projection, verify that refusal leaves that record unchanged, and
require the recorded acknowledge-then-fail behavior. The issue evidence is in
[Goobers#7899](https://github.com/Agent-Clubhouse/Goobers/issues/7899#issuecomment-6373994884).
CI runs `go run ./test/journalequivalence/cmd/equivalence verify-pending` against
GitHub; closing the issue fails this gate until the fixtures become fully equal.

The cancellation fixture represents a daemon drain at a stage boundary, not an
operator abort. Its cloud leg stops a real Temporal worker after the first
stage's finish is durably committed and before the next workflow task is polled.
The five-event journal remains running, matches the local snapshot, and then
continues on a replacement worker. Operator-abort durability is tracked
separately in [Goobers#8088](https://github.com/Agent-Clubhouse/Goobers/issues/8088).

## Canonicalization and independent metadata checks

The shared canonicalizer masks only the established timestamp, run-ID,
host-identity, and duration fields, preserving null and zero-time presence.
Exactly two additional delivery fields are excluded on both sides:

- `runner.emitKey`: assigned by `journalCommitOwner.assignEmitKeys` in
  `internal/engine/journalowner.go`. `opEmitScope` in `internal/engine/emit.go`
  intentionally uses an empty scope for run-level events. Every cloud event
  after the backend-created `run.started` must still have a unique key whose
  run, branch, stage/gate, attempt, and numeric segments agree with that event.
- `terminalCause.causalEmitKey`: the delivery key resolved by livejournal for
  `causalEventSeq`. Its exact projection mirror at
  `Run.Operator.Reliability.TerminalCause.causalEmitKey` is also excluded.
  An independent assertion requires the key to identify the same-run event at
  that sequence; missing, wrong-event, and foreign-run keys fail.

No wildcard exclusion is permitted. Unknown `runner.*` fields fail until their
producer is reviewed and the field is explicitly compared. Nested business
fields with the same names stay in the canonical bytes. Mutations check both
metadata assertions and the exclusion boundaries.

## Running

Use Go 1.26.10, a local PostgreSQL 16 container, and the pinned Temporal CLI:

```sh
GOOBERS_TEST_POSTGRES_URL=postgres://... \
  go test -tags cloudintegration ./test/journalequivalence \
  -run TestCloudJournalEquivalentToLocal -count=1 -timeout=10m
```

The cloud-pg job executes this package and includes it in `cloud-pg-report`, so
missing PostgreSQL configuration or a missing build tag cannot silently skip
the test. The spec spells the test `TestCloudJournalEquivalesLocal`; this package
uses the corrected `TestCloudJournalEquivalentToLocal` name.

The shared canonicalizer's volatility allowlist is path-specific in
`canonical_metadata_test.go`: event `time`/`runId`, typed projection lifecycle
clock/run-ID fields, and the managed branch's embedded current run ID in
`externalRef.id` / `Run.Operator.WorkspaceBranch`. Array paths name only schema
arrays. Business outputs, artifact names, strings containing the run ID, and
untyped objects are compared verbatim; there is no recursive key-name masking.
Unset, empty, zero and populated timestamps remain distinguishable. No
host/duration envelope field occurs in this corpus, so none is implicitly
masked. New locations require an explicit reviewed path.

The tagged parent checks PG configuration before starting its fixtures. Missing
PG fails strict CI; an opted-out local invocation skips the parent. The CI
report rejects skipped descendants of any required tagged test even if its
parent reports success.
