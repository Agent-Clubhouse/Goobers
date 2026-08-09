# FAMILY: pr-remediation

## Sources read
- **A** (instance `goobers`, live): `/Users/masonallen/source/goobers-instances/config/gaggles/goobers/workflows/pr-remediation.yaml` — 914 lines, `dslVersion: "2.0"`, last documented internal sync 2026-08-03 (7dae0bc8), operator tuning through 2026-08-07.
- **B** (instance `goobers-site`): same repo, `config/gaggles/goobers-site/workflows/pr-remediation.yaml` — 346 lines, `dslVersion: "2.0"`, header dated **NEW 2026-08-04**, explicitly "modeled on" A.
- **C-reference** (product repo): `reference-workflows/gaggles/goobers/workflows/pr-remediation.yaml` — 843 lines, `dslVersion: "1.4"`, last touched by commit `09cd7a95` (2026-08-03 19:35 PT), self-labeled "INTENTIONAL LIVE DIVERGENCE" from the live instance.
- **C-acme-web**: no standalone remediation workflow. `config-examples/gaggles/acme-web/workflows/implementation.yaml` folds a single `remediate-ci` task into the main issue→PR pipeline (triggered off `ci-gate`'s `fail` branch).
- **C-{python,dotnet,java}-service**: no remediation shape whatsoever — confirmed by `grep -rn remediat` returning nothing in any of the three. Each is a 4-stage `implement → review → local-ci → local-gate` skeleton that explicitly says it "omits stack-agnostic PR-lifecycle stages" (no PR opening, no CI-poll, no remediation loop at all).
- **Embedded `goobers examples`**: `cmd/goobers/examples.go` reads via `configexamples.WorkflowExamples()`, which is literally `config-examples/` (confirmed in `config-examples/embed.go`). So the embedded CLI catalog = the same C set above; no separate content, and same absence.

## 1. Stage-inventory table

| Stage/Gate | A (goobers) | B (goobers-site) | C-reference | C-acme-web | C-{py,dotnet,java} |
|---|---|---|---|---|---|
| `update-behind-pr` (API-only fast lane) | ✅ | ✅ identical role | ✅ identical role | absent | absent |
| `gather-pr-context` (versioned brief, branch rebind) | ✅ | ✅ identical role | ✅ identical role | absent | absent |
| `gather-ci-failures` (PRR-2 CI diagnostics) | ✅ | absent | ✅ identical role | n/a (inline `remediate-ci` reads `ci-poll`/`local-ci` context directly, no dedicated gather stage) | absent |
| `gather-sibling-context` (overlap classification) | ✅ | absent (explicitly dropped) | ✅ identical role | absent | absent |
| `gather-review-threads` (PRR-3 native review bodies) | ✅ | absent (explicitly dropped) | ✅ identical role | absent | absent |
| `gather-issue-context` (PRR-4 originating issue) | ✅ | absent (explicitly dropped) | ✅ identical role | absent | absent |
| `rebase-pr` (rebase-first, cause classification) | ✅ | ✅ present-but-simpler (no sibling-overlap cause feed) | ✅ identical role | absent | absent |
| `remediation-checkpoint` (stuck-PR / no-progress / per-cause budgets) | ✅ | ✅ present-but-simpler (budgets 3 vs A's 2, no human-comment cause) | ✅ present-but-simpler (missing human-comment cause — see §3) | absent — no checkpoint/budget concept at all | absent |
| `guard-before-agent-context` / `guard-before-implement` / `guard-before-review` / `guard-before-local-ci` / `guard-before-push` (merged-while-running race guards, #1860) | ✅ (5 guards) | absent (explicitly dropped, "closes a race window that matters more for a busier repo") | ✅ identical role | absent | absent |
| `implement` (agentic rework) | ✅ | ✅ identical role (own `site-implementer`/`site-reviewer` goobers) | ✅ identical role | ✅ present-but-simpler (`remediate-ci`, CI-failure only, `minimumIntegrity: unapproved`) | ✅ present-but-simpler (bare repass to `implement`, no CI-evidence stage) |
| `validate-finding-responses` + `finding-responses-gate` | ✅ | absent (explicitly dropped) | ✅ identical role | absent | absent |
| `respond-to-findings` (auditable per-finding publication) | ✅ | absent (explicitly dropped) | ✅ identical role | absent | absent |
| `local-ci` | ✅ (timeout 1800s) | ✅ (timeout 900s, npm/astro stack) | ✅ (timeout 1500s) | ✅ (shared `local-ci`/`local-gate` with implementation) | ✅ |
| `local-gate` (failure-class: pass/fail/infra/escalate) | ✅ | ✅ identical role | ✅ identical role | ✅ identical role (shared) | present-but-simpler (`status-equals`, no infra split, `escalate → @abort`) |
| `push-remediated` (force-with-lease republish + clear label) | ✅ | ✅ identical role | ✅ identical role | n/a — uses `push-branch`/`open-pr` (different mechanism, fresh-PR not republish) | absent |
| `release-claim` / `release-escalated-claim` (explicit claim release, #1860) | ✅ (2 stages) | absent (no PR-claim ledger use at all) | ✅ identical role | n/a | absent |
| `park-escalated` (reviewer `fail` verdict) | ✅ | ✅ identical role | ✅ identical role | ✅ (shared park-escalated) | absent (`@abort` directly) |
| `park-infrastructure-failure` (retryable infra ≠ defect) | ✅ | ✅ identical role | ✅ identical role | present in shared implementation local-gate | absent |
| `park-invalid-finding-responses` | ✅ | absent (no finding-responses concept) | ✅ identical role | absent | absent |
| `checkpoint-gate` + `checkpoint-continue-gate` (escalation-outcome split, #1860) | ✅ (2 gates) | ✅ but collapsed — `checkpoint-gate` fail branches straight to `@escalate` (no `release-escalated-claim` interposed, since B has no claim-release concept) | ✅ identical role | absent | absent |
| Cause vocabulary in `rebase-pr`'s `remediate:` | conflict, substantive, failing-ci, behind-base, sibling-overlap (5) | same 5 (declares `siblingOverlapBudget` unused) | **6**: same 5 **+ human-comment** | n/a | n/a |

## 2. Similarity verdict

A user who copied C (any variant) and stopped there would **not** reach A's reliability for PR remediation. Concretely, what's missing and the production lesson each guard encodes (per the files' own comments):

- **`gather-sibling-context` / sibling-overlap cause** — without it, a remediation cycle can't tell "this PR conflicts because of an unrelated already-merged sibling" from "this PR's own change is broken," so it burns the wrong budget and can loop pointlessly against a moving target.
- **The five `guard-before-*` stages (#1860)** — A's own comment: "a PR that merges or closes mid-cycle can't occupy the remediation slot for the rest of the run." Without these, a long-running remediation session (implement + review + local-ci can be 30+ min) can spend a full agentic cycle on a PR that's already terminal, silently squatting a concurrency slot. This is the single largest structural gap in every C variant and in B.
- **`remediation-checkpoint`'s per-cause budgets (#953/#941, PRR-6)** — direct memory-documented lesson: *"Identical-diff = symptom bucket"* and *"pr-remediation identical-diff re-escalation loop"* (3rd occurrence → escalate). Without independent budgets, "a chronic CI failure no longer burns the same budget as an unrelated conflict" is lost — one flaky cause exhausts the whole remediation allowance for unrelated causes. Acme-web's inline `remediate-ci` has **no budget/checkpoint concept at all** — just a flat `retry.maxAttempts: 2`, which is exactly the "flat repass counter" A's own header says PRR-6 replaced because it couldn't distinguish cause classes.
- **`validate-finding-responses` / `respond-to-findings` (PRR-7)** — A's comment: "so every 'addressed' claim describes work that actually landed" — an auditable, machine-checked accounting that every merge-review finding was actually handled before merge-review re-verifies. Without it, a remediation can silently address 3 of 5 findings and merge-review just burns another cycle discovering that — no C variant has this at all.
- **`gather-review-threads` / `gather-issue-context` (PRR-3/PRR-4)** — closes "the remediator has no PR review-thread context" gap; without it the agentic session works from the raw verdict only, missing native GitHub review-thread resolved/outdated state and the originating issue's acceptance criteria.
- **`failure-class` local-gate with an `infra` branch** — distinguishes a retryable infrastructure failure from a genuine code defect (comment: "so a retryable infra failure no longer burns implementer budget"). `python/dotnet/java-service` still use the older `status-equals` check with no infra split — a flaky runner failure gets treated as a real implementer failure and consumes a repass.

## 3. Divergence direction

**Closest to A: C-reference** (structurally near-identical — same 23 tasks, same 6 gates, same guard-before-* set, same PRR-2/3/4/6/7 stages). **B is second-closest** (same core shape, deliberately narrower). **C-acme-web and C-{py,dotnet,java} are not really in the same family** — they're a different, shallower thing (CI-retry-inline vs. full-PR-remediation).

- **C-reference's gap (human-comment cause) is STALE, not principled.** `git log` shows PR #2395 ("treat a new human comment as a remediation cause") landed in this product repo at `09cd7a95`, **2026-08-03T19:35 PT** — after A's own last-documented sync point `7dae0bc8` (**2026-08-03T06:58 PT**, same day). So on this one axis the *direction is reversed*: the product-repo reference is ahead of the live instance, and A simply hasn't pulled that specific lesson forward yet. Everything else C-reference "lacks" relative to A (cadence `*/5` vs `37 * * * *`, concurrency 5 vs 1, budget `2` vs `3`… wait, budgets in A=2, in reference file — check: reference doesn't show budgets in the diff above, they're identical at "2") is **explicitly principled**: the file's own header calls it "INTENTIONAL LIVE DIVERGENCE," deliberately keeping conservative reference defaults rather than mirroring one operator's tuning.
- **B's gaps (guards, sibling-overlap, review-threads/issue-context, finding-response auditing) are STATED AS PRINCIPLED** in its own header — "deliberately drops," "closes a race window that matters more for a busier repo" — and it explicitly frames each as "a real, available enhancement... if this repo's PR volume ever warrants it." This is a genuine, documented onboarding-scope choice, not staleness. (B is dated 2026-08-04, i.e., cut from A/reference *after* both existed, so it's a considered fork, not an old snapshot.)
- **C-acme-web and C-{py,dotnet,java}'s absence of a standalone remediation workflow is STALE relative to the lesson, not principled about remediation itself.** python-service's file says it "omits stack-agnostic PR-lifecycle stages" as a deliberate polyglot-focus choice — that's principled for *what it's demonstrating* (stack-specific CI command + capability gating), but it means **no shipped example anywhere in C demonstrates the PR-remediation pattern as a reusable workflow**, only acme-web's much narrower inline `remediate-ci` fragment. A new user copying C has no template for `update-behind-pr`, `rebase-pr`, or `remediation-checkpoint` at all — those exist only in A/B/C-reference.

## 4. Parameter deltas that matter

| Param | A | B | C-reference | C-acme-web |
|---|---|---|---|---|
| Trigger cadence | `*/5 * * * *` (lowered 2026-08-07 from every-minute, GH quota) | `*/5 * * * *` | `37 * * * *` (hourly — intentionally not mirrored) | n/a (implementation cadence `3,18,33,48 * * * *`) |
| `maxConcurrentRuns` | 5 | 2 | 1 (intentional) | 1 |
| `maxRunsPerHour` | 900 | 200 | 2 (intentional) | 8 |
| `runControls` | `{}` (engine defaults: 3 repasses / 45m stalled-run) | `{}` | not present in shown diff (implied default) | not applicable |
| Per-cause budgets (`conflict`/`substantive`/`failingCI`/`siblingOverlap`) | 2/2/2/2 | 3/3/3/3 (declares `siblingOverlapBudget` even though B never classifies overlap — command hard-requires it) | 2/2/2/2 + `humanCommentBudget: 2` | none (flat `retry.maxAttempts: 2`) |
| `implement.retry` | maxAttempts 2, backoff 15s, **no `onTimeout: salvage`** (deliberate — comment explains salvage would misfire on a PR's-own-branch worktree) | maxAttempts 2, backoff 15s | maxAttempts 2, backoff 15s, same no-salvage rationale | maxAttempts 2, backoff 15s, `onTimeout: salvage` (correct here — different worktree semantics, fresh branch) |
| `local-ci.timeoutSeconds` | 1800 ("merge-tier suite legitimately runs past the executor default") | 900 (npm/astro stack) | 1500 (#1969: "8.1m at p50" rationale) | inherited from implementation's own `local-ci` (make ci) |
| `push-remediated.retry` | maxAttempts 1, backoff 120s | maxAttempts 1, backoff 120s | maxAttempts 1, backoff 120s | n/a (`open-pr` stage instead, no republish concept) |
| `local-gate` check kind | `failure-class` (pass/fail/infra/escalate) | `failure-class` | `failure-class` | `failure-class` (shared with implementation) |
| Reviewer gate retry | `maxAttempts: 2` (#765, transient copilot-harness failure) | `maxAttempts: 2` | `maxAttempts: 2` | n/a (implementation's `review` gate has no retry override) |
| Claim-release stages | `release-claim` + `release-escalated-claim` (2 dedicated terminal stages, #1860) | none — no PR-claim ledger interaction at all | same 2 stages as A | n/a |
| `dslVersion` | `"2.0"` | `"2.0"` | `"1.4"` | `"1.4"` |

## 5. One-line answer

**Divergent, and the gap is structural completeness, not just tuning: no C variant ships a full PR-remediation workflow — acme-web has only a budget-less, checkpoint-less inline `remediate-ci` fragment, and python/dotnet/java-service have nothing at all — so a user copying C would be missing sibling-overlap classification, all five merged-while-running guards, per-cause stuck-PR budgeting, and auditable finding-response publication; B is the one C-adjacent artifact that reaches most of A's shape, and does so by principled, self-documented omission rather than staleness, while C-reference is structurally complete but one landed lesson (human-comment cause, #2395) behind the live instance as of this comparison.**