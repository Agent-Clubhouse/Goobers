// Package e2e is the TOPOLOGY-INDEPENDENT machinery of the Goobernetes
// distributed e2e proof harness (#3517): the evidence-bundle schema, the
// pass/fail/invalid classifier, and the assertion helpers a live smoke run
// (docs/design/goobernetes-smoke.md) populates once the runner topology
// exists — the YAML agent authoring the smoke workflow's stages, and infra
// deriving the runner set those stages place on.
//
// Originally test/goobernetes (a topology-independent, unit-tested fixture
// harness with no cluster-driving), this package graduated to
// internal/e2e (#3517 follow-up) so it can be imported by the production
// binary: `goobers e2e verify` (cmd/goobers/e2everify.go) fetches a real
// run's recorded data from the read-side and drives these same assertion
// helpers against it, writing the resulting evidence Bundle. A test/ package
// is not importable by convention (and is excluded from the deadcode gate),
// so internal/e2e is this machinery's permanent, product-importable home —
// this is not a relocation of temporary test scaffolding.
//
// # What this package is
//
// Every type and function here is a pure function of DATA — journal events,
// read-model records, runnersolve results, write-API request/response
// shapes — never a live cluster call. Each helper maps to one item of
// goobernetes-smoke.md §4 (S1-S9) or goobernetes-architecture.md §11, is
// grounded against the real contract that item's design already names as its
// observer, and is unit-tested with a FIXTURE standing in for what a live run
// will actually produce. The mapping from a real workflow's stage names to
// the smoke's S1-S9 roles, the six-cell kill-matrix's live injection
// mechanics, and the concrete live-cluster runbook are explicitly OUT of
// scope — those depend on the topology and are seams here (see killmatrix.go's
// CellDriver, and the "TOPOLOGY-PENDING" doc comments throughout).
//
// # Reading order
//
//   - verdict.go — the three-way pass/fail/invalid classifier (D4/§5 rule 2).
//   - evidence.go — the Bundle schema + writer (§5 rule 3: the evidence
//     bundle discipline) that a live run fills, one ObserverResult per S1-S9
//     item.
//   - freshpod.go, oshop.go, handoff.go, artifactmaterialization.go,
//     repass.go, writeapi.go, livevisibility.go, negativecontrol.go —
//     S1-S9's assertion helpers, in that order.
//   - ledgerwindows.go, capabilitygap.go — goobernetes-architecture.md §11
//     items 7 and 8.
//   - killmatrix.go — S6's six-cell driver skeleton and the CellDriver seam.
//   - noexec.go — the §5 rule 1 "no kubectl exec anywhere" guard.
//
// Every exported helper's doc comment cites the smoke-doc section and the
// file:line of the real contract it reads, per the #3517 task's grounding
// requirement.
package e2e
