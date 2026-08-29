# Provider Contract & Conformance — capability-declared providers, test-defined parity

> Status: **draft — for review.**
> Driving epic: #2061 (hero: ADO end-to-end). Related: #2026, #2050, #2059, #2064.
> Author: state-of-repo review follow-up, 2026-07-31.

## 1. Problem

Goobers abstracts forges behind `providers.Provider` (`RepoProvider` + `BacklogProvider` + `TriggerProvider`), with optional capability interfaces (`BranchDeleter`, `PolicyProvider`, `PullRequestReviewThreadProvider`, …) so GitHub-only features "do not widen every backend." The pattern is right, but it has three gaps the 2026-07-30 state-of-repo review made concrete:

1. **Parity is undefined.** GitHub implements everything; ADO implements the read half. The landing surfaces — merge, enqueue, branch-delete, compare — return "unimplemented" (`providers/ado.go:227,587,600,605`), so an ADO workflow cannot complete. Nothing states which gaps are *policy* and which are *debt*.
2. **Gaps are discovered at runtime, sometimes silently.** Optional interfaces are probed by type assertion at the call site. Worst case, a gap fails open: `HasOpenWorkItemBlocker` waves work through a gate on error (#2059). There is no preflight that says "this workflow needs capabilities this provider lacks" before a run starts.
3. **Conformance is asymmetric.** The GitHub↔ADO contract corpus covers shared read surfaces; the landing surfaces have no contract tests because only one provider implements them. Behavior differences (retry semantics — #2026 — pagination, identity) live in per-provider tests, so parity drifts invisibly.

Meanwhile more forges are plausible (Gitea and others have been raised). Adding a third provider under today's rules would triple the ambiguity.

## 2. Goals and non-goals

**Goals**

- One **generic provider contract** covering the git patterns (clone/branch/commit/PR/review/landing) and backlog patterns (work items/comments/claims/states) that workflows consume — provider-neutral in vocabulary and semantics.
- **Capabilities declared, not probed**: each provider declares what it supports; unsupported is a first-class, visible state — never a silent no-op, never fail-open.
- **Conformance corpus as the definition of parity**: a provider "supports" a capability iff it passes that capability's contract tests. Coverage and consistency are driven by the corpus, and gaps are *known*, enumerated, and surfaced.
- **Blessed tier: GitHub and ADO together.** Both reach full conformance on the workflow-required capability set; they move in lockstep from here on (a new capability lands with contract tests + both blessed implementations, or with an explicit declared gap).
- Leave a clean adapter path for community forges (Gitea, …) that may declare gaps but cannot lie about them.

**Non-goals**

- No new forge implementations in this design (Gitea etc. are validation of the shape, not deliverables).
- No change to workflow-visible YAML beyond optional capability requirements (§6).
- Not a V2/cloud item: this is V1 hardening of an existing seam.

## 3. The capability model

### 3.1 Capability keys

Introduce `providers.Capability`, a closed enum of declared surfaces. First cut, grouped by pattern:

| Group | Capabilities |
|---|---|
| repo.core | `repo.clone`, `repo.branch`, `repo.commit`, `repo.push` |
| pr.core | `pr.open`, `pr.list`, `pr.poll`, `pr.close`, `pr.files`, `pr.compare` |
| pr.review | `pr.review.request`, `pr.review.submit`, `pr.review.threads` |
| pr.landing | `pr.merge`, `pr.landing.detect-policy`, `pr.landing.enqueue`, `pr.landing.poll`, `pr.update-branch`, `branch.delete` |
| policy | `repo.policy.read`, `pr.status.publish` |
| backlog | `backlog.list`, `backlog.get`, `backlog.comments`, `backlog.create`, `backlog.update`, `backlog.status`, `backlog.claim`, `backlog.blockers` |
| trigger | `trigger.subscribe` |

Keys are versioned by the corpus (§5), not individually; the enum is append-only.

### 3.2 Declaration

`Provider` gains one method:

```go
// Capabilities reports the surfaces this provider supports. A capability
// listed here MUST pass the conformance corpus for that capability; one
// absent here MUST NOT be reachable at runtime (the dispatcher refuses
// before the provider is called).
Capabilities() providers.CapabilitySet
```

This replaces call-site type assertions as the *authority* (the optional interfaces remain as the implementation seam — declaration and interface satisfaction are cross-checked by a conformance test, so a provider cannot declare what it does not implement, nor implement what it does not declare).

This mirrors two existing, proven patterns: host `SupportMatrix` (#862, release-policy-checked evolution) and runner-capability preflight (#735, declare + schedule-match). Same doctrine, applied to forges.

### 3.3 Semantics of "unsupported"

- Calling an undeclared capability returns `providers.ErrUnsupported{Provider, Capability}` from a dispatch shim — providers never see the call, so per-provider "return an error" stubs (today's `ado.go` pattern) are deleted rather than multiplied.
- **Fail-closed is the only legal gap behavior.** A gate that consults an unsupported read (`backlog.blockers` on a provider without it) must treat the answer as "unknown → do not proceed," fixing the #2059 class structurally: the fail-open path becomes unrepresentable because the gate sees `ErrUnsupported`, not a zero value.

## 4. The landing contract (drives ADO to completion)

The landing abstraction already exists (`DetectMergePolicy` / `EnqueuePullRequest` / `PollMergeQueueEntry`, #758) and `internal/mergepolicy.Land` dispatches on detected policy. ADO maps onto it without new workflow-visible concepts:

| Contract step | GitHub | ADO |
|---|---|---|
| `pr.landing.detect-policy` | branch protection / ruleset (merge queue?) | branch policies on target ref (required reviewers, build validation, status checks) |
| `pr.landing.enqueue` | add to merge queue | **set auto-complete** with policy-satisfaction as the completion condition |
| `pr.landing.poll` | queue entry merged / evicted | PR status: completed (merged) / auto-complete cleared / policy rejection ≙ eviction |
| `pr.merge` | direct merge (no-queue policy) | complete PR without auto-complete |
| `pr.compare` | compare API | diffs commits API (merge-base + file diff) |
| `branch.delete` | ref delete | ref update to zeroed object id |

Contract semantics to pin in the corpus (these are where forges genuinely differ, and where "landed" must mean one thing):

- **Landed** := the provider reports the PR merged into the target ref with a resolvable merge commit/SHA. Auto-complete *set* is not landed; queue *enqueued* is not landed. `pr.landing.poll` is the sole oracle.
- **Eviction is a first-class outcome** on both: GitHub queue eviction and ADO policy-rejection/auto-complete-cleared map to the same `Evicted` result so merge-review's repass loop is provider-neutral.
- **Idempotency**: enqueue/merge called twice must converge (second call observes, not duplicates) — this generalizes #2026's retry work into a contract obligation instead of a per-provider fix.

## 5. Conformance corpus

Extend the existing GitHub↔ADO contract corpus into the parity authority:

- One contract-test suite **per capability**, written against the neutral contract (request/response envelopes + semantic assertions), executed against every provider that declares the capability — fixture-backed by default, live-gated where a real forge is needed (promote `providers/ado_live_test.go` from env-gated-never-runs to a scheduled leg; #2061 child).
- A generated **capability × provider matrix** (like the CLI registry → docs pipeline): `declared / conformant / gap (linked issue) / not-applicable`. Published into docs and asserted in CI so the matrix cannot drift from declarations, and declarations cannot drift from passing tests. This is the "aware of gaps" deliverable — gaps stop being tribal knowledge.
- **Blessed-tier rule, CI-enforced**: GitHub and ADO must be `conformant` on every capability in the workflow-required set (§6); any other divergence needs a linked, labeled gap issue. Community adapters must be conformant on whatever they declare — nothing more, but nothing less.

## 6. Workflow preflight

Workflows (and stages) already imply capability needs; make them explicit and checked before dispatch, in the runner-capability preflight mold:

- Each shipped workflow declares `requires.capabilities` (derived defaults from the stages it uses; overridable in the definition).
- Scheduler preflight refuses to dispatch a run whose required capabilities exceed the connection's provider declaration — the failure is a config-time/park-time message ("workflow `merge-review` requires `pr.landing.enqueue`; provider `gitea` does not declare it"), not a mid-run stage error at hour two.

## 7. Delivery plan (child issues for #2061)

1. **Contract layer**: `Capability`, `CapabilitySet`, `ErrUnsupported`, dispatch shim; GitHub + ADO declare current truth (ADO's declaration will honestly exclude landing — that is the point). Cross-check test: declaration ⇔ interface satisfaction.
2. **Matrix generation + CI gate**: capability × provider doc generated and drift-gated; blessed-tier rule enforced.
3. **ADO landing implementation** against §4: auto-complete enqueue, poll, merge, compare, branch-delete (`providers/ado.go:227,587,600,605`), retiring the stub-error pattern.
4. **Corpus extension**: landing + review-thread + blocker contract suites run against both blessed providers; ADO live test scheduled in CI.
5. **Fail-closed sweep**: #2059 plus an audit of every optional-interface probe site, migrated to the dispatch shim.
6. **Workflow preflight**: `requires.capabilities` + scheduler refusal.
7. **Retry/idempotency contract**: #2026 folded into corpus obligations (method-aware retry, marker idempotency) for both providers.

Ordering: 1–2 first (small, unblocks honest visibility), 3–4 as the bulk, 5–7 parallelizable. `ado.go` satellite split (#2050) rides along with 3.

## 8. Acceptance (epic gate, restated from #2061)

An ADO repo completes issue → curated → implemented → **merged** autonomously in the conformance harness; the capability matrix shows GitHub and ADO fully conformant on the workflow-required set; and both facts are pinned CI gates so parity cannot silently regress.
