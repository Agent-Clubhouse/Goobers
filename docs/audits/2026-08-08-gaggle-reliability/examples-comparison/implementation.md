## FAMILY: implementation — A vs B vs C

**Source resolution first:** `goobers examples show implementation` prints `config-examples/gaggles/acme-web/workflows/implementation.yaml` **byte-identically** (`config-examples/embed.go` `//go:embed gaggles/acme-web/workflows/implementation.yaml`; catalog = implementation, backlog-assignment, backlog-curation, work-nomination — `default-implement.yaml` is *not* embedded). So "embedded examples implementation" and "acme-web implementation" are one artifact, not two.

### 1. Stage / gate inventory

`✔`=identical role · `~`=present but simpler · `—`=absent

| stage / gate | **A** impl | **A** impl-critical | **B** site | **C** acme-web (=`goobers examples`) | **C** reference-workflows | **C** py/java/dotnet |
|---|---|---|---|---|---|---|
| query-backlog | ✔ trust+ready, `excludeLabels: in-review,goobers:critical`, `respectAssignee` | ✔ `requireLabels: ready,critical` | ~ no respectAssignee, no critical excl. | ~ no respectAssignee, no critical excl. | ~ same as acme | — (`start: implement`) |
| gather-implement-context | ✔ maxHotFiles 100 | ✔ | — | ✔ | ✔ | — |
| implement (agentic) | ✔ `agent:model`, retry 2/15s, `onTimeout: salvage`, PROVIDER_ACTION_REQUIRED clause | ✔ + "CRITICAL → minimal targeted fix" | ✔ (site-implementer) | ~ **no `agent:model`**; **+ `minimumIntegrity: maintainer` + `contextFrom` allowlist**; no PROVIDER_ACTION clause | ✔ `agent:model` **+ integrity/contextFrom** | ~ no integrity, no contextFrom, no salvage-context prose |
| remediate-ci (unapproved-grade consumer) | **—** | **—** | — | **✔** | **✔** | — |
| local-ci | ✔ `make ci test-integration-strict`, syncBase, **1800s** | ✔ same | ~ npm chain + `sync-reference --check`, **900s** | ~ `make ci`, syncBase, **no timeoutSeconds** | ~ `make ci`, **1500s** + p50/p90 rationale | ~ stack cmd, no timeout |
| push-branch | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| open-pr (+`opened`, runIdFooter) | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| ci-poll | ✔ `provider:pr:write` | ✔ | ✔ | ✔ | ✔ | — |
| close-out → `status: in-review` | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| park-escalated → needs-remediation, `@escalate` | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| park-needs-human → `@abort` | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| **gate** review (agentic) | ✔ pass/needs-changes/fail→park-needs-human/escalate→park-escalated | ✔ | ✔ | ✔ | ✔ | ~ fail+escalate → `@abort` (no parking) |
| **gate** local-gate | ✔ `failure-class` (+`infra` branch) | ✔ | ✔ | ✔ | ✔ | ~ `status-equals`, **no infra branch** |
| **gate** open-pr-gate (`output-equals opened=true`) | ✔ | ✔ | ✔ | ✔ | ✔ | — |
| **gate** ci-gate | ✔ `pollIntervalSeconds: 10`, fail→implement, timeout→park-escalated | ✔ | ✔ | ~ **no pollInterval**, fail→**remediate-ci** | ~ same as acme | — |
| dedicated critical lane | ✔ separate file | — | — | — | — | — |

Onboarding stubs (for completeness): `config-examples/.../default-implement.yaml` = query-backlog → implement (terminal, no gates, `trustLabel: "goobers"`); `internal/instance/starter/gaggles/example/workflows/default-implement.yaml` = + push-branch + open-pr (#2447, 2026-08-07); `internal/instance/quickstart-v1/.../quickstart.yaml` = + review + `make ci`. None have gates or parking.

### 2. Similarity verdict — could a C-copier reach A's reliability?

**acme-web / reference-workflows: yes, ~90% of the way.** The full PR lifecycle, all four gates, the escalation taxonomy, and the two-parking-lane split are all there with A's own comments intact. What they'd be missing:

| missing in acme-web/ref | production lesson it encodes |
|---|---|
| `local-ci: timeoutSeconds` (acme-web only; ref has 1500) | **#1969** — 10m `boundedwait.DefaultTimeout` vs `make ci` p90 9.5m ⇒ SIGQUIT reported as ordinary CI failure ⇒ implementer finds no defect ⇒ **identical-diff escalation loop**. acme-web inherits the default and is exposed to exactly this. |
| `respectAssignee: "true"` | #1820 — manual routing gate; without it the lane claims *any* ready+approved issue |
| `excludeLabels: goobers:critical` + the critical lane | #2268-class — a critical-tagged ready item matches both lanes; whichever tick fires first wins, defeating the dedicated pool |
| `test-integration-strict` tier | #2368 — declared-dependency integration validation |
| `ci-gate pollIntervalSeconds: 10` | poll cadence tuning; default cadence lengthens CI-poll wall clock |
| `agent:model` on implement **and** on the implementer goober | #294 — Copilot model auth injected per-invocation through the credential seam. acme-web's `goobers/implementer/goober.yaml` grants only `repo:push`. |
| readiness budgets | see §4 |

**python/java/dotnet: no — and not trying to.** They are `implement → review → local-ci` only. Their own headers say so explicitly (dotnet: *"omits the stack-AGNOSTIC PR-lifecycle stages… nothing about them differs for .NET"*; *"a real deployment would park the issue for a human, as acme-web does — omitted here"*). A user copying one gets no claim, no PR, no parking, and `status-equals` instead of `failure-class` — an infra flake becomes a re-implementation repass.

**B (goobers-site): deliberate 85% clone.** Its header names exactly what it dropped: *"trimmed of the accumulated hardening… no gather-implement-context, no respectAssignee, no critical lane."* Everything safety-shaped (4 gates, both parks, `failure-class`, `open-pr-gate`, timeout→park) survived.

### 3. Divergence direction & vintage

`reference-workflows/gaggles/goobers/workflows/implementation.yaml` and `config-examples/gaggles/acme-web/workflows/implementation.yaml` are **twins at the same commit** — last touched `6f5ba52f` (2026-08-03, ADO PR parity #2391). acme-web is the de-Go-ified, comment-abridged clone (`npm run ci`, gaggle `acme-web`, drops `agent:model`, adds `expectedOutputs: changed-files`). Both are CI-gated (`internal/workflow/implementation_test.go` pins acme-web; `reference_workflows_test.go` pins the reference set).

**reference-workflows is NOT a current mirror of live.** Its own header admits partial sync: *"INTENTIONAL LIVE DIVERGENCE: the checked-in reference keeps two runs/day, maxConcurrentRuns=1, maxRunsPerHour=1, maxOpenPRs=1."* But the drift is **bidirectional**, and that part is not declared:

- **Ref ahead of live:** `remediate-ci` + `minimumIntegrity: maintainer` + `contextFrom` allowlists (TBH-4 / #1885, landed 2026-07-29, moved into reference 2026-08-02 `002df00c`). The live instance's last sync was from main `34359cf4` — which **descends from** `6f5ba52f` — so live had these available and did not take them. Live's `implement` therefore has *no* integrity floor (`MinimumIntegrity` empty = "no admission policy") and receives every accumulated pointer, including provider-authored CI evidence, straight into the implementer session. **The shipped example is stricter than production here.**
- **Live ahead of ref:** critical lane, `respectAssignee`, `test-integration-strict`, 1800s local-ci, `pollIntervalSeconds`, `dslVersion 2.0`, `runControls: {}`.

**Verdict on principle vs staleness:**

| C variant | vintage | call |
|---|---|---|
| reference-workflows | 2026-08-03 | **Principled on budgets** (declared), **stale on lane topology** (no critical lane, no respectAssignee, no integration tier) — and *ahead* on integrity. Mislabeled as a mirror. |
| acme-web / `goobers examples` | 2026-08-03 | Mostly principled onboarding scope; **one genuine staleness**: no `local-ci timeoutSeconds`, i.e. it does not carry #1969, the lesson even its own twin carries. |
| python-service | 2026-08-02 (`e22ffc35`, TSN-3) | **Principled** — scope declared in-file. |
| java-service | 2026-08-02 (`02cfc4c9`, TSN-2) | **Principled.** |
| dotnet-service | **2026-07-21 origin, last touched 2026-07-25** (`96b43817`, DSL lifecycle) | **Oldest C file by two weeks**; content-frozen since PLY-4 #1093. Principled in scope but the most weathered. |
| acme-web default-implement | 2026-07-25 | **Stale** — starter got the #2447 fix (push-branch + open-pr, 2026-08-07 `2763ec23`); the acme-web copy still terminates at `implement` with an aspirational `expectedOutputs: pull-request`. Divergent onboarding promise between two shipped "default-implement" files of the same name. |

Closest to A: **reference-workflows**, then **acme-web** (near-tie; ref wins only on `local-ci timeoutSeconds` and `agent:model`).

### 4. Parameter deltas that matter

| param | A impl | A crit | B site | C acme | C ref | C py/java/dotnet |
|---|---|---|---|---|---|---|
| trigger | `* * * * *` | `* * * * *` | `*/5 * * * *` | `3,18,33,48 * * * *` | `17 8,20 * * *` | `manual` |
| maxConcurrentRuns | **5** | 2 | 2 | 1 | 1 | 1 |
| maxRunsPerHour | **900** | 900 | 20 | 8 | 1 | 8 |
| maxOpenPRs | **10** | 3 | 3 | **unset** | 1 | unset |
| runControls | `{}` (engine: 3 repasses / 45m stall) | `{}` | `{}` | unset (same effective) | unset | unset |
| local-ci timeout | **1800s** | 1800s | 900s | **default 600s** ⚠ | 1500s | default 600s |
| local-ci retry | 1 | 1 | 1 | 1 | 1 (+ "deliberately still 1" note) | 1 |
| implement retry | 2 / 15s + salvage | same | same | same | same | same |
| ci-gate poll | 10s | 10s | 10s | default | default | n/a |
| implement caps | `repo:push`,`agent:model` | same | same | **`repo:push` only** | both | `repo:push` only |
| open-pr / ci-poll caps | `provider:pr:write` (#2391/#2328) | same | same | same | same | n/a |
| minimumIntegrity | **unset** | unset | unset | `maintainer` (+`unapproved` remediate-ci) | same as acme | unset |
| dslVersion | **2.0** (preview) | 2.0 | 2.0 | 1.4 (current) | 1.4 | 1.4 |

`maxOpenPRs` unset in acme-web is the highest-leverage omission after the timeout: the #353 comment explaining *why* the cap exists (siblings branched off `origin/main` become mutually un-mergeable as a set) survives **only** in reference-workflows and A. acme-web deleted the comment *and* the field.

### 5. One-line answer

**Similar — structurally near-identical, and the gap is operational tuning plus one real staleness: acme-web/`goobers examples` reproduces A's entire stage graph and all four gates verbatim, but ships without `local-ci timeoutSeconds` (#1969's identical-diff escalation loop), without `maxOpenPRs` (#353), without `agent:model`, and without A's critical-lane/`respectAssignee` routing — while, in the other direction, both shipped copies carry a `minimumIntegrity`/`remediate-ci` integrity split that the live instance never adopted.**

Files read (all absolute):
- A: `/Users/masonallen/source/goobers-instances/config/gaggles/goobers/workflows/implementation.yaml`, `.../implementation-critical.yaml`, `.../goobers/implementer/goober.yaml`
- B: `/Users/masonallen/source/goobers-instances/config/gaggles/goobers-site/workflows/implementation.yaml`
- C: `/Users/masonallen/source/Goobers/.clubhouse/agents/Goobers-Special-Agent/config-examples/gaggles/{acme-web/workflows/implementation.yaml,acme-web/workflows/default-implement.yaml,acme-web/goobers/implementer/goober.yaml,python-service/workflows/python-implementation.yaml,dotnet-service/workflows/dotnet-implementation.yaml,java-service/workflows/java-implementation.yaml}`, `.../config-examples/embed.go`, `.../reference-workflows/gaggles/goobers/workflows/implementation.yaml`, `.../internal/instance/starter/gaggles/example/workflows/default-implement.yaml`, `.../internal/instance/quickstart-v1/gaggles/example/workflows/quickstart.yaml`
- Gates on the C files: `.../internal/workflow/implementation_test.go`, `.../internal/workflow/reference_workflows_test.go`