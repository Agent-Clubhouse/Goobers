## FAMILY: merge-review (+ merge-review-critical)

### Sources resolved

| ID | Path | Lines | dslVersion | Vintage |
|---|---|---|---|---|
| **A** | `/Users/masonallen/source/goobers-instances/config/gaggles/goobers/workflows/merge-review.yaml` | 684 | 2.0 | last touched 2026-08-03 (`f93ea26`), header says synced from main `34359cf4` |
| **A′** | `…/config/gaggles/goobers/workflows/merge-review-critical.yaml` | 403 | 2.0 | 2026-07-29 lane fork |
| **B** | `…/config/gaggles/goobers-site/workflows/merge-review.yaml` | 243 | 2.0 | 2026-08-04 new, audited 2026-08-08 |
| **C1** | `…/Goobers-Special-Agent/config-examples/gaggles/acme-web/workflows/merge-review.yaml` | 473 | 1.4 | 2026-08-03 (`80370b21`, #2393) |
| **C2** | `…/Goobers-Special-Agent/reference-workflows/gaggles/goobers/workflows/merge-review.yaml` | 661 | 1.4 | 2026-08-03 (`80370b21`, #2393) |
| **C-embedded** | `config-examples/embed.go` `//go:embed` set | — | — | **merge-review NOT embedded** |
| **C-other** | `examples/`, `internal/instance/{starter,quickstart-v1,demo}`, `config-examples/gaggles/{python,dotnet,java}-service` | — | — | **no merge-review at all** |

---

### 1. Stage/gate inventory

`≡` = identical role and wiring · `~` = present but simpler · `—` = absent

| Stage / gate | A | A′ | B | C1 acme-web | C2 reference |
|---|---|---|---|---|---|
| reconcile-post-merge (`continueOnError`) | ≡ | ≡ | — | ≡ | ≡ |
| pr-select | ≡ | ≡ | ≡ | ≡ | ≡ |
| check-issue-staleness + issue-staleness-gate | ≡ | ≡ | ≡ | ≡ | ≡ |
| gather-sibling-context | ≡ | ≡ | ~ (no `overlappingSiblingsCsv`, no `scopeGateParked`) | ≡ | ≡ |
| review (agentic, retry 2) | ≡ | ≡ | ~ (needs-changes→apply-verdict, no `escalate`) | ≡ | ≡ |
| elect-lander + elect-gate | ≡ | ≡ | — | ≡ | ≡ |
| apply-verdict | ≡ | ≡ | ~ (no `electionPolicy`, no overlap/scope threading, no `priorityDispatchRequested`) | ≡ | ≡ |
| advisory-verdict gate | ≡ | ≡ | — | ≡ | ≡ |
| published-verdict gate | ≡ (→scope-gate) | ≡ | ~ (→merge-pr direct) | ≡ | ≡ |
| scope-gate | ≡ | ≡ | — | ≡ | ≡ |
| merge-opt-out-gate | ≡ | ≡ | ≡ | ≡ | ≡ |
| merge-pr + merge-gate (`land-outcome`) | ≡ | ≡ | ~ (`enqueued`→"", `fail`→"") | ≡ | ≡ |
| queue-watch + queue-opt-out-gate + queue-gate | ≡ | ≡ | — | ≡ | ≡ |
| post-merge | ≡ | ≡ | ≡ | ≡ | ≡ |
| record-merge-refusal | ≡ | ≡ | — | ≡ | ≡ |
| **Total stages / gates** | 10 / 10 | 10 / 10 | 6 / 5 | 10 / 10 | 10 / 10 |

**A vs C1 vs C2 are structurally byte-identical.** Comment-stripped diff over all three yields *only* metadata/tuning lines — no stage, gate, branch, capability, policyAction, expectedOutput, or timeout differs. A′ differs from A only by name/displayName, `headPrefixes` (`implementation-critical/`) and `maxConcurrentRuns: 2`.

---

### 2. Similarity verdict

**A user who copies C1 or C2 gets A's exact merge machinery.** Every production lesson encoded in A is present verbatim in the shipped examples, with the originating issue numbers in the comments:

| Guard | Lesson encoded (from the files' own comments) | In C? |
|---|---|---|
| `reconcile-post-merge` as `start:` | #1162 — a PR that merged *after* queue-watch gave up had no re-entry path; cleanup/close-out/fan-out never ran | yes |
| `queue-watch` `pollTimeoutSeconds: 25m` + `timeout: 30m` | #886/#884 — queue build outlasts the 10-min default stage timeout; poll clamps inside the stage budget, so declaring one silently caps the other | yes, same values |
| `merge-gate` `check: land-outcome` (not `output-equals merged==true`) | #758 — plain equality conflates "enqueued, not merged" with refusal | yes |
| `record-merge-refusal` + `demote-pr` | #950 — a lander that repeatedly can't merge at an unchanged head must be demoted so its cluster drains around it; keyed by head SHA so it self-heals | yes |
| `scope-gate` after `published-verdict` | #1313/#1814 — oversized PR must still get **fresh review findings** (so remediation can converge) but be parked short of merge; the feedback loop was the bug | yes |
| `elect-lander` `resultFile: election.json` | outputs reach the runner **only** through a declared result file; omitting it made every `expectedOutput` silently vanish and crashed the run | yes, with the comment |
| `elect-gate` both branches → `apply-verdict` | the elected→merge-pr bypass posted no verdict comment, so merge-pr exited 1 every cycle and satisfied "reviewed favorably" from a hardcoded input | yes, C1 carries the *longest* version of this postmortem |
| `github:branch:delete` on merge-pr + queue-watch | #1075/#581 — grant listed as satisfied but never wired; cleanup failed closed on every merge, branches accumulated | yes |
| `merge-opt-out-gate` / `queue-opt-out-gate` before the real gate | #758 — a late `goobers:no-merge-review` label must drop the PR cleanly, and `skipped` must never reach `record-merge-refusal`'s mutation path | yes |
| `review` gate `retry.maxAttempts: 2` | #765 — a copilot session that wrote no verdict file is transient | yes |
| `check-issue-staleness` before the agentic gate | #2340 — don't review against a stale copied spec | yes (landed in *both* on 2026-08-03) |
| `apply-verdict.electionPolicy` **must match** `elect-lander.electionPolicy` | apply-verdict re-derives the election because a single-hop `inputsFrom` edge fails closed on the review-gate path | yes, both pinned `fifo` |

**What they'd be missing is not in the workflow — it's the reviewer persona.**

`config-examples/gaggles/acme-web/goobers/reviewer/instructions.md` (67 lines) has **zero occurrences** of `sibling`, `holistic`, `cross-PR`, `overlap`, `blockingPrs`, or `merge-review` — yet its `goober.yaml` registers it `workflows: [implementation, merge-review]`, and `internal/workflow/merge_review_test.go` asserts that registration. C2's reviewer (193 lines) has the full `## Holistic mode (merge-review's review gate)` section; even **B's** minimal `site-reviewer` (109 lines) has one. So the embedded/documented example ships a structurally perfect merge machine driven by a reviewer that was never told cross-PR review exists. The workflow's own comment names the consequence: *"this is where the cross-PR value lives; without it the review degrades back to single-diff and catches nothing cross-cutting."* Concretely: no `cross-pr-blocked` findings → `elect-lander` sees no blockers → `elect-gate` is inert → the whole election half of the graph is dead weight; and pure file overlaps get filed as `substantive`, routing good PRs into remediation.

Second gap: **merge-review is not in the binary.** `config-examples/embed.go` embeds exactly 4 workflows (`implementation`, `backlog-assignment`, `backlog-curation`, `work-nomination`) plus 4 goober dirs. `goobers examples list|show` therefore cannot surface merge-review. Nor do `examples/` (hello-world, ios-simulator), `internal/instance/starter` (`default-implement`), `quickstart-v1` (`quickstart`), or `demo` (`demo.yaml` — its `merge-preview` stage is unrelated), nor any polyglot gaggle. **The CLI-discoverable example set stops at open-pr.** You only reach merge-review by browsing the repo's `config-examples/` tree or reading its README §"Merge and review".

---

### 3. Divergence direction

**C2 (reference-workflows) is closest to A** — it is a *sync* of A, not an independent example, and it says so in its own header (`# INTENTIONAL LIVE DIVERGENCE: the checked-in reference keeps the hourly :23 cadence and maxRunsPerHour=2`). Sync vintage: **current, not stale.** Both C1 and C2 were last updated 2026-08-03 by `80370b21` (#2393, ADO PR closure → `provider:pr:write`), and both received #2340 the same day; A's own last content sync was `968e7a2`/`fe32e99` on 2026-08-03. Structural drift between live and shipped: **zero**.

**C1 (acme-web) is a rename-and-retarget of C2** — differs only by `gaggle:`, `maxConcurrentRuns: 1`, and dropping `goobers/tutor/` from `headPrefixes`. It is not an independently authored onboarding example.

Simplification is **principled** where it exists and **absent** where it matters:
- **B** is principled and self-documenting: its header explicitly enumerates each dropped feature (merge queue, election, scope-gate, refusal-demotion) with the reason ("this repo has no GitHub merge queue configured", "only matters once multiple PRs mutually block each other") and points at A as the upgrade path. It also adds a lesson A doesn't state: no `escalate` branch on the review gate, because escalation records no verdict artifact and `apply-verdict` would publish a review with nothing behind it.
- **C1/C2 are not simplified at all** — which is the problem. A first-time user copying C1's merge-review is handed 10 stages, 10 gates, `github:pr:merge`, `github:branch:delete`, and a merge-queue watcher on day one. There is no graduated variant. The README's "Merge and review" row and the "Copying subsets safely" note ("Add `merge-review` only with the reviewer still present and after PR/merge credentials, webhook or schedule delivery, and merge policy are configured") are the only ramp.

**One live-instance regression found in the opposite direction:** A's reviewer persona is *older* than C2's. `config/gaggles/goobers/goobers/reviewer/instructions.md` was last touched 2026-07-25 (`e1786a9`) and still says *"infer overlap risk from what you know"* and *"File a `substantive` finding"* for cross-PR overlap — while A's workflow emits `overlappingSiblingsCsv` (#990) and C2's reviewer has the corrected rule: *"Do NOT file `substantive` for a pure file overlap — that would send a perfectly-good PR into remediation to reconcile a collision the system resolves by sequencing."* C2's reviewer also carries the full class taxonomy (`conflict` / `substantive` / `missing-tests` / `scope-creep` / `contract-change`), integer `blockingPrs` for automated routing, and the "CI is deliberately not a finding" rule — none of which the live instance's reviewer has. **The shipped reference is ahead of the live instance on the persona.**

---

### 4. Parameter deltas that matter

| Parameter | A | A′ | B | C1 | C2 |
|---|---|---|---|---|---|
| `dslVersion` | 2.0 | 2.0 | 2.0 | **1.4** | **1.4** |
| triggers | schedule `* * * * *` only | same | schedule `*/5 * * * *` | webhook(`pull_request`) + `23 * * * *` | webhook + `23 * * * *` |
| `maxConcurrentRuns` | 5 | 2 | 2 | **1** | 4 |
| `maxRunsPerHour` | 900 | 900 | 200 | **2** | **2** |
| `runControls` | `{}` (engine defaults: 3 repasses / 45m) | `{}` | `{}` | **absent** | **absent** |
| `headPrefixes` | `masons-goobers/implementation/` | `…implementation-critical/` | `goobers-site/implementation/` | `goobers/implementation/,goobers/docs-updater/` | `…,goobers/tutor/` |
| `authorScope` | `goobers` | `goobers` | `goobers` | **`any`** | **`any`** |
| queue-watch `pollTimeoutSeconds`/`timeout` | 25m / 30m | 25m / 30m | n/a | 25m / 30m | 25m / 30m |
| review `retry.maxAttempts` | 2 | 2 | 2 | 2 | 2 |
| `electionPolicy` (both stages) | fifo | fifo | n/a | fifo | fifo |
| reconcile `maxPullRequests`/`lookback` | 10 / 168h | 10 / 168h | n/a | 10 / 168h | 10 / 168h |
| `continueOnError` | reconcile only | reconcile only | none | reconcile only | reconcile only |
| capabilities | identical across A/A′/C1/C2: `github:pr:write`, `github:issues:write`, `github:branch:delete`, `provider:pr:write`, `github:pr:review`, `github:pr:merge` | | B drops nothing on the stages it keeps | | |

Deltas that will actually bite a copier:
- **`dslVersion: 1.4` vs live `2.0`.** `internal/workflow/v_current/types.go` pins `DSLVersion = "1.4"`, `v_next/types.go` pins `"2.0"`. 2.0 is the **preview** tier — `api/validate/validate.go` refuses it unless the Manifest carries the preview-features annotation. Examples correctly ship stable; the live instance is opted in. Principled, not stale.
- **`maxRunsPerHour: 2` + `maxConcurrentRuns: 1` (C1).** Copied verbatim, merge-review runs at most twice an hour, one at a time. Fine as an onboarding default; A had to reach 5/900 and documents why (`60 would become the real bottleneck if cycles turn over faster than once every 5 minutes`) and warns that `<=0` is not "unlimited" — it falls back to a **lower** default of 10.
- **`authorScope: any` in C.** Enables mixed-company advisory mode; A deliberately keeps `goobers` and says so ("a deliberate policy choice for multi-repo/multi-author setups, out of scope for this single-repo, goobers-authored-only instance"). A copier on a repo with human PRs gets advisory reviews on stranger PRs by default.
- **Missing `runControls: {}` in C.** #1698's stalled-run safety net. Engine defaults likely still apply, but the declaration is A's/B's explicit opt-in and the examples don't teach it.
- **Webhook trigger in C.** A removed it on 2026-08-03 (`f93ea26`, "drop the webhook trigger, poll-only") because the host has no publicly reachable endpoint and `webhook.secret` is unset. A copier without webhook delivery gets an inert trigger — harmless, but the README lists "webhook delivery and/or scheduling" as a prerequisite without saying which is the safe default.

---

### 5. One-line answer

**Similar — near-identical, in fact: the shipped `merge-review` YAML is A's file, and the gap is not the workflow but (a) the acme-web `reviewer` persona has no holistic/cross-PR section at all, so the election half of the graph runs blind, and (b) `merge-review` is absent from every CLI-discoverable example surface (`goobers examples`, `examples/`, all three scaffolds, all polyglot gaggles) — the embedded catalog stops at open-pr.**

Key paths:
- `/Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent/config-examples/embed.go` — the 8 `//go:embed` lines; merge-review is not among them
- `/Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent/config-examples/gaggles/acme-web/goobers/reviewer/instructions.md` — 67 lines, no holistic mode, yet registered for merge-review
- `/Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent/internal/workflow/merge_review_test.go` — CI test pinning both shipped copies to the full post-merge chain (why they can't rot structurally)
- `/Users/masonallen/source/goobers-instances/config/gaggles/goobers/goobers/reviewer/instructions.md` — live persona, last touched 2026-07-25, behind C2's on #990 overlap classification