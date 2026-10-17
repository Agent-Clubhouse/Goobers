# WIP takeover: bounded Backprop run viewer (#4664)

Repository: `Agent-Clubhouse/Goobers`  
Owned worktree: `worktrees/backprop`  
Branch: `codex/backprop-run-view`  
Base: `395cf74f45765173ccb2e092aa11f57a1477c019`  
Implementation head: `1b9b9751972ffdb8525f65440326f2b8bc62d955`

This checkpoint adds only this handoff note after the implementation head. Work stopped for handoff at Mason's request. The bounded slice is implemented locally; the broader #4664 remains open. This branch is not approved for merge, deployment or release. No PR was created for this lane.

## Scope

`GET /api/v1/runs/{run}/attribution?gaggle={gaggle}` reads the authoritative persisted `creditgraph.RunRecord`, current journal evidence and validated human labels. The Portal Backprop run tab distinguishes signed scores from unsigned responsibility shares, recorded analysis from current evidence availability, and run phase from human correctness. It preserves provenance, uncertainty and label revision.

Reads are rooted in the trusted instance `os.Root` and use pinned handles plus `safeopen.OpenRegularInRoot`. Only the selected scoped/legacy run locations are visited; existing runtime names and compatibility aliases are supported. Exact journal/blob references are verified. Missing and committed-redacted evidence are disclosed without substituting new bytes for old analysis.

Bounds: eight-second route budget; 64 MiB total input; 8 MiB record and encoded response including newline; 1 MiB identity/labels; 64 KiB schema manifest; 100,000 journal events; 10,000 contribution/evidence entries. Portal renders 50 contributions per page and at most 100 entries per detail list. Limits produce explicit availability without a partial record.

No cohort scans, attribution recomputation, fault-audit writes, label writes, migrations, rotation/remediation controls, provider/model calls or live infrastructure changes. No dependency on unmerged #7531 or #7540 was assumed. Full campaign integration and any overlaps with other lanes must be resolved by the successor.

## Validation already completed

Commands below used the campaign's bounded runner. No new tests were run for this documentation-only checkpoint.

At implementation head `1b9b9751972ffdb8525f65440326f2b8bc62d955`:

- PASS: `go test ./internal/readservice ./internal/apicontract -run 'TestRunAttribution|Test.*OpenAPI|Test.*Generated' -count=1` (includes runtime names and compatibility aliases).
- PASS: `go vet ./internal/readservice ./internal/apicontract`, `go run ./test/complexitygate`, and `git diff --check`.
- PASS under approved local-loopback escalation: `go test ./internal/httpapi -run 'TestServerServesTLSOnConfiguredCertificate|TestDefaultLoopbackPostureUnchanged|TestEventStreamServesUnderAuthenticator' -count=1`.

Before the final OpenAPI/query contract and runtime-name follow-ups:

- PASS: focused readservice/journal/creditgraph/httpapi attribution, rooted-read, bounded-event and label-decoder tests.
- PASS: `make verify-fast` (format, no-phone-home, whole-tree vet and command builds). Affected tests/vet/complexity reran at the implementation head above; the complete fast gate did not rerun there.
- PASS: Portal `npm run typecheck`; `npm run test -- --maxWorkers=2 src/components/RunAttributionPanel.test.tsx src/api/wireContract.test.ts src/pages/RunPage.test.tsx src/routing.test.ts` (74/74). Fresh lockfile dependencies were installed; Portal source did not change afterward.
- Earlier full suites passed for readservice, journal, creditgraph and apicontract. The full httpapi suite stopped on sandbox loopback bind failures/panic; its three blocked cases subsequently passed as shown above, but the complete package suite after that panic is not claimed passed.

Independent source review found no remaining blockers at the implementation head, including all five initial fixes: filesystem containment/FIFO/races, event allocation before count enforcement, quadratic UI evidence lookup, redaction semantics and label validation. Independent focused tests passed at the preceding reviewed head; the reviewer could not acquire a slot for the final three-file follow-up. Author tests at the final implementation head passed as recorded above.

Not completed here: full `make ci`, whole-tree race/coverage/platform qualification, browser/real-daemon E2E, production-size inventory or deployment validation. No merge approval exists. Source-review readiness does not establish full merge readiness.

## Evidence and next steps

The originating task retains safe status/review summaries in `campaign/receipts/backprop-local-status.md` and `campaign/reviews/backprop-final.md`, and raw local logs under `campaign/receipts/backprop-*.log`. These are outside this repository and not reproducible from this repository; raw logs/caches are deliberately not published. This note preserves the material results and limitations.

1. Verify this branch's checkpoint SHA and review the diff from the recorded base.
2. Reconcile current main and other campaign branches without assuming held dependencies landed; preserve this checkpoint if further work uses another branch.
3. Run the required full repository gate and platform/browser checks before requesting merge approval. Recheck the source if integration changes it.
4. Keep #4664 open for cohort exploration, rotation and remediation design; do not imply those controls shipped.

Rollback for this slice is removal of the additive route/panel. Its reads never alter persisted analyses or labels. Publishing this checkpoint is authorized solely for takeover, not merging, releasing, changing credentials or deploying.
