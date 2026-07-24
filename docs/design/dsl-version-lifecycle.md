# Design: DSL Version Lifecycle & Multi-Version Runtime

> Status: **Draft for review — not implemented** · Area prefix: `DVL` (new) · Milestone: **Versioning & Releases** (#12)
> Companion to: [`versioning-and-compatibility.md`](./versioning-and-compatibility.md) — **this doc resolves that doc's Open Question §5.2** ("version the DSL independently, or app-SemVer + registry-as-authority?") in favour of an *independently-versioned, per-workflow-pinnable DSL with multiple interpreters coexisting in one binary.*
> Architecture: [`docs/ARCHITECTURE.md`](../ARCHITECTURE.md) §11 (substrate-neutral workflow core)
> Grounded in: `internal/workflow/{compile,machine}.go`, `api/v1alpha1/`, `api/validate/validate.go`, `internal/configsync/loader.go`, `internal/instance/config.go`

---

## 0. TL;DR

Today the DSL has exactly one version string — `apiVersion: goobers.dev/v1alpha1` — pinned as a
JSON-Schema `const`. It never changes, and the *meaning* of the fields under it changes silently
underneath every daemon build. Config authors (including our own live instance) have no version to
pin to, no statement of what a given binary supports, and no way to keep an old workflow running
*exactly as it did* while the shipped definitions move on.

This design adds three things the current versioning epic (#426) deliberately left open:

1. **An independently-versioned DSL** (`dslVersion: <major>.<minor>`) with defined change semantics
   per bump level, **pinned per-workflow** so one workflow can upgrade while its neighbours don't.
2. **A version *support lifecycle*** — `preview → supported → deprecated → unsupported` — that the
   **host binary declares** and enforces at config-load time. Distinct from, and layered on top of,
   the per-*feature* support levels in #426/#427. (An **LTS** tier is deliberately *not* planned at
   this time — see Non-Goals §9 — but the level is left as a future extension point.)
3. **Multi-version runtime coexistence** — the daemon carries **N interpreters, one per supported
   DSL minor**, and routes each workflow to the interpreter for its pinned version. We accept
   forking/copying interpreter code per minor as the *price of the "supported means it works as-is"
   guarantee*, exactly as Rust editions and Kubernetes CRD served-versions do.

The `goobers` **binary** is maintained **forward-only**: while the team is small, effort goes into
fixing the main app forward, not cutting backport releases of old binaries. You get fixes by
upgrading forward, and your pinned DSL versions keep working across that upgrade (that's what §3.4
buys). This is a *resourcing* stance, not an architectural promise — revisit it if/when there are
more contributors.

---

## 1. Why this exists — the incident that names the gap

On 2026-07-18 the live instance's hand-maintained workflow config
(`~/source/goobers-instances/goobers`) was reconciled to the shipped selfhost reference in a single
287-line `merge-review.yaml` diff. The headline finding:

> the fork routed the review gate `pass → merge-pr`; the shipped definition had moved to
> `pass → apply-verdict → merge-pr` (#825's native-review writeback). **No verdict was ever
> published and `merge-pr` never merged — 24 consecutive merge failures** before anyone noticed.

`goobers validate .` reported **OK (6 workflows)** against a current-main binary the whole time. The
drift was not a schema violation — every field was individually valid. It was a *semantic* drift
between what the instance's YAML said and what the binary's stages now expected. Validation can't
catch it because there is no version anywhere to disagree about.

That is the loud example. The quiet ones are worse because they compound. In the same reconcile,
`pr-remediation.yaml` needed only this:

```diff
       expectedOutput:
         - base
         - isBehindBase
         - hasSubstantiveFindings
+        - hasFailingCI          # gather-pr-context now also emits this…
       next: rebase-pr
 ...
         hasSubstantiveFindings: hasSubstantiveFindings
+        hasFailingCI: hasFailingCI   # …and rebase-pr must thread it through inputsFrom
```

A purely-internal producer↔consumer contract change between two deterministic stages. Invisible to
`validate`. A hand-maintained fork silently under-behaves (remediates only labelled PRs, never
red-CI ones) with **zero error, ever**. As we add features, plumb new fields between stages, and
tune stage vocabularies, *every* instance config drifts a little more from *every* binary it wasn't
authored against — and today nothing measures the distance.

**The thesis:** a config language with no versions is a language where every release is a silent,
undeclared breaking change to everyone who didn't rebuild from the reference. We already feel this
with one internal consumer. It "only gets worse over time," and it gets categorically worse the day
a consumer outside this repo pins to us.

---

## 2. Relationship to the existing versioning epic (#426)

This is **additive**, not a replacement. The two models compose on orthogonal axes:

| | #426 feature registry (VER-1…4) | This doc (DVL) |
|---|---|---|
| **Unit** | a single named feature (`gate.evaluator.human`) | a whole DSL version (`1.4`) |
| **Question it answers** | "is *this field* GA / deprecated / removed?" | "does this binary *support version 1.4 at all*, and how?" |
| **Granularity** | fine (per field/enum/stage-kind) | coarse (per author-facing release) |
| **Author touchpoint** | warnings on specific fields | one `dslVersion:` pin per workflow |
| **Runtime consequence** | validate warns/errors | **routes to a different interpreter** |

The feature registry stays the source of truth for *what changed between versions* (and generates
the per-version changelog/feature-matrix). DVL adds the coarse pin authors actually hold, the
host's support declaration, and the runtime that keeps old pins working. Where #426 §5.2 leaned
"app-SemVer + registry-as-authority, revisit if consumers need to pin the DSL independently" — this
doc is that revisit, and the answer is **yes, pin independently.**

`apiVersion: goobers.dev/v1alpha1` keeps its Kubernetes meaning: the *resource group/version* of
the CRD shape. `dslVersion` is a new, finer field for the *language* semantics. (See §6.1 for why we
don't overload `apiVersion` for this — briefly: K8s `apiVersion` bumps are heavyweight full-CRD
revisions with conversion webhooks; the DSL's language moves far more often than its resource shape,
so it needs its own lighter-weight axis.)

---

## 3. Core model

### 3.1 DSL SemVer — what a version *means* to an author

DSL versions are `MAJOR.MINOR` (patch is a runtime concern, see §3.5 — a config author never pins a
patch). The contract, stated from the **config author's** point of view (not a library API's):

- **MAJOR** (`1.x → 2.0`) — *rare, deliberate, breaking.* Field removals/renames, changed defaults,
  changed stage semantics, restructured graph rules. An author must actively migrate. We expect
  these on the order of **once a year or less**, and each is accompanied by a `goobers fix` migrator
  (§7) and an overlap window where both majors are supported.
- **MINOR** (`1.3 → 1.4`) — *frequent, mostly-additive.* New stage/gate/trigger kinds, new optional
  fields, new stage-output vocabulary, relaxed constraints. **May** carry a small, announced
  behavioural change, but the default posture is "a `1.3` workflow keeps meaning what it meant."
  This is the level the `merge-review` and `pr-remediation` drifts above would have lived at.
- **PATCH** — *no schema or author-visible semantic change.* Bug fixes in the interpreter/runtime.
  Never pinned by config; you get patches by upgrading the binary (§3.5).

The bright line, enforced by the #429 CI guard extended to versions:

> A field's *meaning* or *presence* may only change across a **MAJOR** bump, or via a deprecate→remove
> cycle spanning ≥1 released MINOR. Anything an existing workflow can observe changing is breaking,
> and breaking changes are gated on a version bump the author opts into.

### 3.2 Per-workflow version pin — independent upgrade

Each workflow declares the DSL version it was authored against:

```yaml
apiVersion: goobers.dev/v1alpha1   # resource shape (K8s group/version) — unchanged
kind: Workflow
dslVersion: "1.4"                  # NEW — the language contract this file is written to
metadata:
  name: merge-review
spec: ...
```

- **Independent** — `merge-review` can be on `1.4` while `implementation` is still on `1.2` in the
  same instance/gaggle. There is no instance-wide DSL version; the pin is the workflow's own.
- **Explicit, not inferred** — a missing `dslVersion` is not "latest." During a transition window it
  defaults to the lowest still-supported version with a `DVL001` warning ("pin your version"); after
  that window a missing pin is a load error. (Inferring "latest" is the Compose-`version`-less trap —
  see §5.5 — and would silently re-introduce exactly the drift we're fixing.)
- **Upgrading is an author action** — bumping `1.2 → 1.4` is a diff the author makes (ideally via
  `goobers fix --to 1.4`, §7), reviewed like any other change. You never get silently upgraded by
  restarting the daemon.

### 3.3 Version support lifecycle — declared by the host

Every DSL version the binary knows about sits at exactly one **support level**. The set of levels
and the *host binary* owns the mapping (a `SupportMatrix` compiled into the binary, printable via
`goobers versions`, §7):

| Level | Loads? | Author signal | Guarantee |
|---|---|---|---|
| **preview** | opt-in only | `DVL010` info | unstable; may change or vanish next minor. Off unless the instance explicitly enables preview versions. |
| **supported** | yes | silent | works as-authored; the normal state. |
| **deprecated** | yes, warns | `DVL020` warning | still runs *exactly as before*; names the target version + the release it becomes unsupported. |
| **unsupported** | **no — load error** | `DVL030` error | the interpreter has been removed; the workflow fails to load until migrated. |

> **No LTS tier at this time** (§9). A `supported-LTS` level — a subset of supported versions with
> an extended, published window — is the **likely first follow-up for the DSL** (config authors want
> a "pin it and leave it for a long time" version sooner than the binary needs one), and the
> `SupportMatrix` design leaves room for it. But we are **not** committing to LTS windows in this
> pass. Today there is one `supported` level with one window (§3.3.1).

Load-time behaviour is enforced in the config loader (`internal/configsync/loader.go`) /
`api/validate`, so it surfaces on `goobers validate`, `goobers up`, and `goobers status` uniformly
(reusing the `Severity`/`Issue`/coded-warning channel #434 already built).

#### 3.3.1 The support *window* (the "how long" promise)

The lifecycle is only worth something if the durations are promised, not vibes. The ratified policy
(borrowed from Kubernetes' API deprecation policy, §5.1) is:

- A **supported** minor stays loadable (supported or deprecated) for **at least 3 minor releases**
  after the release that supersedes it. For example, a version superseded in `v1.1.0` cannot become
  unsupported before `v1.4.0`.
- A version must spend **≥1 released minor in `deprecated`** before it may go `unsupported`. No
  version jumps straight from supported to a load error; a version deprecated in `v1.3.x` cannot
  become unsupported before `v1.4.0`.
- The window is a *promise about the interpreter's continued existence*, which §3.4 is what actually
  makes cheap.
- `SupportMatrix` entries retain their ordered, release-stamped lifecycle history. CI validates the
  compiled-in matrix against both floors and the matrix executed from the latest reachable canonical
  SemVer tag. Released entries and history are append-only, and a version can become `unsupported`
  only when that tagged matrix already marks it `deprecated`; adding both transitions in one change
  does not satisfy the released-minor window. Before the first tag, no version may become unsupported.

### 3.4 Multi-version runtime coexistence — N interpreters in one binary

This is the load-bearing idea and the one the current epic doesn't have. "Supported means it works
as-authored" is only real if the **code that interpreted `1.2` is still in the binary, unchanged,
when the binary also knows `1.4`.**

**Mechanism.** The workflow core (`internal/workflow`, already pure/deterministic/substrate-neutral
per ARCHITECTURE §11) becomes **version-scoped**. Concretely:

```
internal/workflow/          → thin version router + shared, version-agnostic types
internal/workflow/v1_2/     → compiler + machine for DSL 1.2 (frozen)
internal/workflow/v1_3/     → compiler + machine for DSL 1.3 (frozen)
internal/workflow/v1_4/     → compiler + machine for DSL 1.4 (current)
```

- The router reads `dslVersion`, checks the `SupportMatrix`, and dispatches to the matching
  interpreter package. Unknown/unsupported → the §3.3 load error.
- **We accept copy-forward.** When we cut `1.4`, we branch `v1_3/` → `v1_4/` and evolve `v1_4/`;
  `v1_3/` is **frozen** and only ever gets a runtime *patch* fix (a genuine interpreter bug), never a
  feature or semantic change. This is deliberate duplication in exchange for a hard guarantee. Rust's
  compiler does exactly this across editions (§5.6); K8s CRDs serve multiple versions off one binary
  (§5.2). The alternative — one interpreter with version-conditional branches everywhere — is how you
  get the silent drift back, because a change "for 1.4" inevitably leaks into 1.2's behaviour.
- **What's shared vs forked.** The compiled `Machine`/`Definition` runtime contract (state-machine
  execution, digests, WF-016 run-pinning) stays shared and version-agnostic — a run still pins one
  compiled machine for its life. What forks is **compilation and validation**: the mapping from YAML
  fields → compiled machine, i.e. the part that encodes "what these fields mean." A `1.2` workflow
  compiles through `v1_2/`'s rules and runs on the shared executor.
- **Bounded cost.** We don't keep interpreters forever — we keep the *supported set* (§3.3). When
  `1.2` goes `unsupported`, `v1_2/` is deleted. Steady state is ~3–5 interpreter packages, which is
  cheap Go code, fully tested by frozen golden fixtures per version.

> **Design tension to resolve in review:** how much can be shared before a "shared" change silently
> alters an old version's behaviour? The rule is: shared code may only contain things that are
> *definitionally* version-invariant (the executor's state-walk, digest algorithm). The moment a
> behaviour is version-observable, it lives in the versioned package. See Open Question §8.1.

### 3.5 Runtime (binary) versioning — forward-only maintenance

The `goobers` **binary** is SemVer'd independently (`goobers --version`, REL-1/#431). Its maintenance
policy is **forward-only, as a resourcing decision**:

- **While the team is small, effort goes into fixing the main app forward — we do not cut backport
  releases of old binaries.** A fix ships in the next binary release; you get it by upgrading
  forward. This is a stance about where our limited maintenance attention goes, *not* an
  architectural promise; revisit it if/when there are more contributors.
- Upgrading the binary **must not** break your pinned DSL versions — that's the entire point of §3.4.
  A newer binary still carries the older interpreters that are still in its `SupportMatrix`.
- A **PATCH** (§3.1) is a binary release that changes interpreter/runtime behaviour to fix a bug
  *without* changing any version's author-visible contract. If a "fix" changes what a workflow
  observes, it isn't a patch — it's a new DSL minor or major.
- This is the standard interpreter/runtime posture (Go, Rust, browsers): move forward; the language
  contract is what's preserved across the move.

> **Deferred corner — must-fix inside a frozen interpreter.** Once §3.4's coexistence exists, there
> is a theoretical case (a correctness/security bug *inside* a frozen `v1_2/` that authors can't
> quickly migrate off) that the forward-only stance doesn't cleanly answer — and, per prior art (§5),
> the freeze-old-behaviour + fix-forward *combination* has no off-the-shelf precedent. We are **not
> designing for this now**: with a small team and few coexisting versions, the answer is "fix it
> forward and migrate." If it ever bites, the likely shape is a contract-preserving patch (guarded by
> that version's golden fixtures) or an out-of-band version revocation — but that's a *later* problem,
> not a v1 deliverable. Tracked lightly as Open Question §8.7.

---

## 4. How this model catches the incident (§1) at the door

| Failure today | With DVL |
|---|---|
| `merge-review` fork routed `pass → merge-pr`; binary expected `pass → apply-verdict → merge-pr`; 24 silent merge failures. | The fork is pinned `dslVersion: "1.2"`. Binary ships `v1_2/` unchanged → it runs *exactly as the 1.2 author intended*, OR (if `apply-verdict` is a 1.4-era stage the 1.2 graph can't express) `v1_2/` compilation flags the missing stage at **load time**, not after 24 merges. Either way: no silent divergence. |
| `pr-remediation` silently missed `hasFailingCI` threading. | The new stage-output vocabulary is a `1.4` minor feature. A `1.2`-pinned workflow simply doesn't have it and behaves as its author expected; upgrading to `1.4` is an explicit `goobers fix` diff that *adds the threading*, reviewed. |
| `validate` says OK while semantics rot. | `validate` now also reports each workflow's pinned version and its support level against *this* binary — so "this instance pins 4 workflows at 1.2, which this binary marks **deprecated**, unsupported after 2.9" is a visible, actionable line, not a surprise 3 releases later. |

The instance's config stops being "a fork we periodically rediscover has drifted" and becomes "a set
of workflows each pinned to a version this binary either supports, deprecates, or refuses — and says
so out loud."

---

## 5. Prior art

> _Filled from a dedicated research pass; see the Prior-Art appendix at the end of this doc. Each
> system below is mined for one transferable mechanism; the traps are called out because most of
> these patterns have a way they *don't* fit a config language._

- **5.1 Kubernetes API deprecation policy** — the gold standard for *how long* a version is
  promised. (→ §3.3.1)
- **5.2 Kubernetes CRD multi-version + conversion** — `versions:` list, `served`/`storage`,
  conversion webhooks: one binary serving many versions. The direct model for §3.4. (→ §3.4)
- **5.3 Terraform provider/schema versioning + state upgrade functions** — `required_version`
  pinning + per-schema `schema_version` with upgrade functions. Model for §3.2 pinning + §7
  migration. (→ §3.2, §7)
- **5.4 GitHub Actions** — why workflow YAML almost never breaks, and how `runs.using`
  (node16→node20) deprecates a *runtime* under stable *syntax*. Mirrors our binary-vs-DSL split.
  (→ §3.5)
- **5.5 Docker Compose `version:` → version-less** — the cautionary tale: they *removed* the version
  field. Why "infer latest" is a trap for us specifically. (→ §3.2)
- **5.6 Rust editions** — one compiler, all editions forever, per-crate `edition`, `cargo fix
  --edition`. The closest analogue to §3.4's copy-forward interpreters. (→ §3.4, §7)
- **5.7 Protobuf/Avro schema evolution** — compatibility-by-rules vs compatibility-by-version;
  why we choose explicit versions over "just never break the schema." (→ §3.1)
- **5.8 SemVer** — the MAJOR/MINOR/PATCH contract, and where it's awkward for a *language* vs a
  library (→ §3.1: we drop author-facing PATCH).

---

## 6. Mechanism sketch (grounded in the tree)

### 6.1 Where `dslVersion` lives and why not `apiVersion`

`apiVersion: goobers.dev/v1alpha1` is the Kubernetes group/version of the **resource shape**
(`api/v1alpha1/`, the CRD in `config/crd/bases/goobers.dev_workflows.yaml`). Bumping it is a
full-CRD revision with conversion machinery — appropriate for rare shape changes, far too heavy for
the language cadence (§3.1 minors land often). So `dslVersion` is a distinct, lighter field on the
Workflow object. It also stays cleanly separate from the existing `Definition.Version int`
(`internal/workflow/machine.go:43`) — that's the monotonic *run-pin* (WF-016), not a language
version.

### 6.2 The support matrix

A small compiled-in table, the single authority the binary declares:

```go
// internal/workflow/support.go (new)
type Level int
const ( Preview Level = iota; Supported; Deprecated; Unsupported ) // SupportedLTS deferred (§9)

type SupportTransition struct { Level Level; SinceVersion string }
type VersionSupport struct {
    Level Level
    UnsupportedAfter string
    Replacement string
    History []SupportTransition
}
type SupportMatrix map[string]VersionSupport // "1.2" -> current support + ordered history
```

Printed by `goobers versions`, consumed by the loader (§6.3) and the router (§3.4). Generated /
cross-checked against the #427 feature registry so the two can't disagree about what a version
contains.

### 6.3 Load-time enforcement

In `internal/configsync/loader.go` (and mirrored in `api/validate` for the offline `validate`
path), after parsing each Workflow: read `dslVersion` → look up `SupportMatrix` → emit the §3.3
outcome through the existing coded-warning channel (`DVL0xx`). `unsupported` → `Error` (fails the
load, like a schema violation). `deprecated`/`preview` → `Warning`/info, non-fatal.

### 6.4 Version router

`internal/workflow.Compile` becomes a dispatcher: `Compile(def)` reads the pinned version and calls
`v1_2.Compile` / `v1_3.Compile` / … The chosen interpreter returns the same shared `*Machine`. Runs
execute unchanged on the shared executor. Per-version **golden fixtures** (a frozen corpus of
`{yaml → compiled-machine-digest}` per version) are the regression net that proves a frozen
interpreter *stayed* frozen.

---

## 7. Migration & tooling

- **`goobers versions`** — print this binary's `SupportMatrix` (version × level × unsupported-after).
- **`goobers fix --to <version>`** — mechanical migrator, one version step at a time (Rust `cargo fix
  --edition` / Terraform state-upgrade model). Emits a reviewable diff; never auto-applies on daemon
  start.
- **`goobers validate`** — additionally reports, per workflow, its pin and support level against the
  running binary (this is the line that makes drift *visible*).
- **Per-version feature matrix** (extends VER-4/#430) — `docs/feature-matrix.md` gains a version axis:
  what each supported DSL version contains, and the delta between versions.
- **Release notes** (extends REL-3/#433) — every release states support-matrix changes: what newly
  went `deprecated`/`unsupported`, and the migration path.

---

## 8. Open questions

- **8.1 Shared vs forked boundary (§3.4).** Exactly which code is "definitionally version-invariant"
  and safe to share across interpreters vs. must be copied? Getting this wrong re-opens the drift.
  Proposed: share only the executor + digest; fork everything from YAML→machine. Needs a concrete
  audit of `internal/workflow` against that line.
- **8.2 Cost ceiling.** How many interpreters do we carry before the copy-forward tax outweighs the
  guarantee? Strawman: cap the supported window so steady-state is ≤5. Revisit if that's painful.
- **8.3 `dslVersion` default during the transition window.** Lowest-supported-with-warning, then
  hard error (§3.2) — confirm the window length and the exact cutover release.
- **8.4 Do gaggle/goober/manifest objects get their own `dslVersion`, or does the Workflow's pin
  cover the whole gaggle?** Leaning: version the Workflow (that's where semantics live); other
  objects follow the CRD `apiVersion`. Needs confirmation against `internal/instance/config.go`.
- **8.5 Interaction with run-pinning (WF-016).** A run already pins a compiled machine + digest for
  life. Confirm the version router sits *before* that pin (compile-time), so an in-flight run is
  wholly unaffected by a support-level change mid-flight.
- **8.6 Preview opt-in granularity.** Per-instance setting (leaning) vs per-workflow field — same
  question the companion doc left open (#426 §5.1); resolve once for both preview *features* and
  preview *versions*.
- **8.7 Must-fix on a frozen version (deferred, §3.5).** Once coexistence exists, a correctness/
  security bug *inside* a frozen interpreter that authors can't quickly migrate off has no clean
  answer under forward-only maintenance (and no off-the-shelf precedent per §5). **Explicitly out of
  scope for v1** — with a small team and few coexisting versions the answer is "fix forward + migrate."
  Parked here so it isn't forgotten if the version count grows.

## 9. Non-goals

- **No LTS support tier in this pass** (§3.3). Scoped out for now, but with a clear ordering when we
  come back to it: an LTS **DSL version** is the near-term need (authors want a long-lived pin
  sooner), and an LTS **service/app** release is a much-later "maybe" (gated on more contributors +
  the forward-only stance below relaxing). The `SupportMatrix` leaves room for the level either way.
- **No backport releases of old binaries.** Binary maintenance is forward-only as a *resourcing*
  decision while the team is small (§3.5) — not an architectural guarantee.
- Not versioning the **HTTP read API** (`internal/readservice`) — that has its own `APIVersion` and
  is out of scope here.
- Not changing **run-pinning** (WF-016) or the compiled-machine/digest contract — those stay shared
  and version-agnostic (§3.4).
- Not shipping a conversion *webhook* (K8s-style server-side conversion). Our migration is an
  author-run `goobers fix` diff (§7), not silent server-side rewriting — deliberately, so upgrades
  stay reviewable.

## 10. Issue breakdown (milestone #12 — all filed **unapproved**, pending this doc's review)

- **[EPIC] DVL** (#860) — DSL Version Lifecycle & Multi-Version Runtime (this doc).
- **DVL-1** (#861) — `dslVersion` field on the Workflow CRD + parse/plumb (no behaviour yet).
- **DVL-2** (#862) — `SupportMatrix` type + compiled-in table + `goobers versions` CLI.
- **DVL-3** (#863) — Load-time support-level enforcement (loader + validate; `DVL0xx` codes; preview/deprecated/unsupported behaviour).
- **DVL-4** (#864) — Version router: split `internal/workflow` into a dispatcher + first versioned interpreter package (`v_current`), with per-version golden fixtures.
- **DVL-5** (#865) — Second interpreter (copy-forward drill): cut a `v_next`, freeze `v_current`, prove both compile independently — validates the coexistence model end-to-end.
- **DVL-6** (#866) — `goobers fix --to <version>` migrator scaffold (one-step, diff-emitting).
- **DVL-7** (#867) — Support-window *policy* doc + CI guard (extends #429): ≥3 minor releases loadable after supersession, no straight-to-unsupported, ≥1 minor deprecated. (No LTS window — deferred, §9.)
- **DVL-8** (#868) — Per-version feature matrix + release-note support-delta (extends #430/#433).
- **DVL-9** (#869) — Forward-only binary-maintenance policy (resourcing stance): `--version` semantics + PATCH-means-no-contract-change guard, documented (extends #431). The frozen-version must-fix corner (§8.7) is explicitly *not* in scope.

---

### Appendix A — Prior-Art notes

Eight systems, each mined for one transferable mechanism and its trap. §5 maps each into the design.

**A.1 Kubernetes API versioning (alpha→beta→GA).** Stability lives *in the version string*
(`v1alpha1`/`v1beta1`/`v1`). The [deprecation policy](https://kubernetes.io/docs/reference/using-api/deprecation-policy/)
Rule #4a gives hard floors: **GA served ≥ 12 months or 3 releases** and *never removed within a
major*; **Beta ≥ 9 months or 3 releases**; **Alpha 0 releases** (removable without notice). Rule #3:
never deprecate toward a *less* stable version. *Lesson:* encode the promise in the name; publish
release-floor windows (→ §3.3.1). *Trap:* manifests **do** change meaningfully across versions
(`extensions/v1beta1` Ingress → restructured `networking.k8s.io/v1`) — "same kind, new version" is
not drop-in.

**A.2 Kubernetes CRD multi-version + conversion — the core pattern.** A CRD lists `versions:`, each
with `served` (exposed) and `storage` (persisted); **exactly one** is storage. One binary serves
`v1beta1` and `v1` at once, converting on read via `spec.conversion.strategy` = `None` (identical
schema, rewrites only `apiVersion`) or `Webhook` (real conversion logic). Per-version `deprecated:
true` + `deprecationWarning:` surface at parse; safe removal is a strict sequence (no clients →
`served:false` → migrate stored objects → drop). [[docs](https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definition-versioning/)]
*Lesson:* separate **served** (interpreters that run) from **canonical/storage** (one on-disk form)
with an explicit conversion boundary — the direct model for §3.4. *Trap:* `None` only works when
schemas are *literally identical*; a canonical form must be a **superset** of every served version
or round-trips silently drop fields.

**A.3 Terraform.** Three axes: plugin protocol (`5`/`6`), provider semver (`aws 5.42.0`), and a
per-resource integer **state `schema_version`**. Config pins via `required_version` /
`required_providers` constraints; `init` writes exact versions to `.terraform.lock.hcl`; upgrades
only on `init -upgrade`. Old state is migrated forward, one step at a time, by registered
[StateUpgraders](https://developer.hashicorp.com/terraform/plugin/framework/resources/state-upgrade).
*Lesson:* a monotonic version on the persisted artifact + **forward-only upgrade functions** *is*
our forward-fix-only model (→ §3.2, §7). *Trap:* forward-only is clean only because old schemas are
inert data — see A.8/§3.5 for when the frozen thing has live behaviour.

**A.4 GitHub Actions.** The workflow schema is **effectively unversioned** (no `version:` key) —
stability comes from append-only discipline. Versioning is pushed down to the action ref
(`@v4`) and the action *runtime* (`runs.using: node16|node20|node24`); the runner ships multiple
Node runtimes and dispatches per action, so a job mixes runtimes. Deprecation targets the *runtime*,
never the syntax (node16 warned 2023-09, [node20 default 2024-06](https://github.blog/changelog/2024-03-06-github-actions-all-actions-will-run-on-node20-instead-of-node16-by-default/),
[node20→24 removed 2026-09](https://github.blog/changelog/2025-09-19-deprecation-of-node-20-on-github-actions-runners/)),
with end-of-job warnings + temporary env escape hatches. *Lesson:* mirrors our binary-vs-DSL split —
version/deprecate the runtime under a stable surface (→ §3.5). *Trap:* works because their surface
is a loose imperative shell; a richly-typed DSL **cannot** stay append-only forever, so we *will*
need real language versions where Actions avoided them.

**A.5 Docker Compose `version:` → version-less — the anti-pattern.** The
[Compose Spec](https://github.com/compose-spec/compose-spec/blob/main/04-version-and-name.md) merged
the v2/v3 lineages and **dropped** `version:` (now "informative only"; v2 warns "attribute 'version'
is obsolete"). Compose always validates against the **latest** schema regardless. *Lesson:* a version
field that gates a schema matrix is a maintenance tax *if* your language is purely additive. *Trap
(this is the model to avoid):* they could delete `version` only because they made almost no breaking
changes and accept "latest wins" — the **opposite** of our "run old workflows exactly as-is."
Copy the *reason* additive-only worked, never the version-less endpoint. This is why §3.2 makes the
pin mandatory and rejects "infer latest."

**A.6 Rust editions — closest analogue.** Per-crate `edition = "2015|2018|2021|2024"`; **one
compiler supports every edition, and crates of different editions link in one build.** Core
guarantee: **old editions compile forever — never dropped.** Migration is opt-in/mechanical (`cargo
fix --edition`). [[guide](https://doc.rust-lang.org/edition-guide/editions/)] *Lesson:* per-unit
pin + one binary hosting all versions + never-drop, with automated forward-migration as convenience
(→ §3.4, §7). *Trap:* Rust keeps this cheap because editions share one `std`/type system — only the
parser/lint *frontend* forks. Literally forking whole interpreters (which we've accepted) pays the
guarantee **without** the shared-core discount; budget for N interpreters + cross-version test
surface (→ Open Q §8.2).

**A.7 Protobuf / Avro schema evolution.** Deliberately **no version on the message** — compatibility
is disciplined edits: immutable field numbers, add-only, never reuse a number, `reserved` tombstones
for removed fields; unknown fields preserved. [[proto3](https://protobuf.dev/programming-guides/proto3/),
[dos-donts](https://protobuf.dev/best-practices/dos-donts/)] *Lesson:* for the *data payload* inside
manifests, prefer compatible-by-construction evolution over minting a version per field addition;
reserve version bumps for genuine semantic breaks (→ §3.1). *Trap:* this governs *data*
compatibility, not *behaviour* — two protos can be wire-compatible while a field *means* something
new. Don't let "we can add fields compatibly" excuse skipping a version bump when *interpretation*
changes.

**A.8 SemVer.** `X.Y.Z`: MAJOR = breaking, MINOR = compatible addition, PATCH = compatible fix.
[[semver.org](https://semver.org/)] *Lesson:* use the *vocabulary* (MAJOR = new interpreter;
MINOR/PATCH = same interpreter) so authors instantly grok risk. *Trap (the subtlest, most
load-bearing point):* semver's "public API" is defined for a library *you call*; a DSL's API is *the
documents the interpreter accepts and how it interprets them*. Two frictions — (1) semver assumes
consumers move *toward* latest, but configs want an old document frozen against old rules forever
(that's **editions**, not "everyone upgrades"); (2) adding a keyword is "additive" for new files but
makes them **unparseable by older interpreters** — breaking *from the old interpreter's view*.
**Decide compatibility per-direction, per-interpreter — never from the author's chair.**

**A.9 A gap with no precedent (parked, not solved).** Our combination — freeze old behaviour **and**
maintain the binary forward-only — is not done in full by any surveyed system: Rust never drops,
Terraform's frozen schemas are inert data, K8s just removes. The unanswered case is *a must-fix bug
inside a frozen interpreter authors can't quickly migrate off.* Since binary maintenance is
forward-only by *resourcing* choice (§3.5) rather than architectural fiat, we don't design for this
now — the v1 answer is "fix forward + migrate." Parked as Open Q §8.7 for if the version count ever
grows enough to make it real.
