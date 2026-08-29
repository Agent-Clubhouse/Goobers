# Onboarding first-value ladder & DSL authoring support (#435, #2431, #2430)

Status: draft — for review. Filed 2026-08-07 from a nine-dimension recon of the
onboarding journey, the init/setup code surface, the DSL and its validation,
the shipped agent-context assets, the backlog, prior design art, test/e2e
coverage, stack neutrality, and CLI-surface coherence — plus PO directives
recorded in §1. First deliverable: this doc. Decision-free mechanical children
are filed immediately (§8, wave B); decision-bearing children are filed only
after the rulings in §7 land.

**Implementation status (updated 2026-08-08):** this is a point-in-time
recon snapshot, so its current-state analysis dates as Wave A/B items land —
§8's "landed" list is the source of truth for what's shipped since filing,
not the narrative prose in §3-§7. One correction already folded in: #2549
(R4's local-ci wiring, below) landed the same day this doc was filed and has
moved from Wave B to Wave A accordingly (#2636).

## 0. Verdict up front

**Onboarding is not missing — it is broken at every rung of a ladder that
already shipped, and it re-breaks because the repo's drift guards pin
generated-vs-source but never source-vs-behavior.** The quickstart encodes a
graduated ladder (demo → quickstart template → guided instance → production
examples); guided init, the demo tour, the sample app, the agent toolkit,
registry-generated CLI docs, and a genuinely excellent config validator all
exist. But at recon time: the install rung dereferenced releases that have
never been published, `init --guided` failed on every fresh clone (#2513, fixed
by PR #2539), both first-PR rungs could not produce the promised PR
(#2447/#2449), the env-validation rung has no aggregate command and is broken
on stock macOS for claude-code (#2515/#2516/#2517/#2070), and the CLI's own
help lied about what init creates (#2446, fixed by PR #2525). None of this was
caught because no test composes the rungs, no test runs init anywhere but an
empty temp dir, prose guides have zero executable gating, and golden tests
faithfully pin wrong text.

So this design is **fix-forward, not greenfield**, with three thrusts:

1. **One spine, own-repo-first (§3).** The primary path is: install → init →
   `goobers doctor` (new, flagless, aggregates every environment check) →
   connect *your* repository (one choreography that closes #2449's gap) →
   hello-world in *your* repo (read-only backlog + real build gate, then a
   real agentic PR — #2447 resolved by giving the starter the push/open-pr
   stages quickstart@v1 already demonstrates) → customize (agent toolkit,
   scaffolds, examples). The hermetic demo remains a side rung and the
   installer must stop forcing guided setup (#2448); both are explicitly
   deprioritized per PO direction.
2. **DSL authoring support that cannot drift (§4-§5).** Close the
   loads-but-misbehaves seams (placeholder configs that validate clean, the
   unvalidated gaggle↔instance repo join, warn-only tool/skill refs, the
   inert-field ledger, free-form task inputs), give instance.yaml the schema
   and structured diagnostics the config/ tree already enjoys, collapse the
   four hand-synced CLI authorities into the registry, and generate the DSL
   field reference + agent context from the schemas and registries that
   already exist — then extend the proven regen-diff/contract-test discipline
   so every one of these surfaces breaks CI when it lies.
3. **A full-ladder regression gate (§6).** A Go-native `test/onboardingvalidate`
   job composes install → init → doctor → connect (fake GitHub) → daemon →
   agentic hello-world PR (stub harness) from seams that already exist in-tree,
   so the canonical journey can never silently regress again. Per-stack
   off-the-shelf proof rides the same gate (Go first; Node/.NET/Java/Python
   from the fixtures already maintained under test/e2e/testdata).

Terminology stays as-is (PO ruling): no renames — the glossary, CLI help
topics, and docs teach `instance`, `gaggle`, `goober`, `workflow`, `harness`.

## 1. PO directives recorded (2026-08-06/07)

- **Keep existing names**; fix confusion with docs/UX, not renames.
- **The generic public repo is the target** — new users onboarding their own
  repos on common stacks; the maintainer's live instance is out of scope and
  will re-sync itself once this lands.
- **Own-repo path is the spine.** The hermetic "not my repo" demo is a future
  side stage; most users will blow past it.
- **Off-the-shelf must work for common langs** (.NET, Swift, TypeScript, Java,
  Python, Go) and **customization must be easy in many forms**: hand-authoring,
  the user's own agents/skills, or shipped helpers. Docs and reference
  workflows must stand alone; the agent-context pack is an enabler, not a
  requirement.
- A GHCP PAT exists in CI if a real-agent example run is worth one
  manual-dispatch workflow; **nightly runs optional**, hermetic-by-default.
- Deliverable format: design-doc PR + review-gated issues; direct PRs
  (authored, taken to green, merged) are authorized for decision-free fixes.

## 2. Ground truth (recon 2026-08-07, verified in code)

### 2.1 Broken today on the canonical path

| Rung | Defect | Ref | State |
|---|---|---|---|
| Install | Zero releases/tags have ever been published; every platform guide's download+verify block 404s. Release machinery (packaging, install.sh, SHA256SUMS, doc adaptation) exists and is tested. | release/, docs/guides/quickstart-macos.md | **Decision 7.1** |
| Install | install.sh unconditionally chains `init --guided`; cancel misreports install failure; demo unreachable from installer. | #2448, release/install.go:150 | **Decision 7.2** |
| init | Fresh-clone `config/` collision: guided refused pathlessly, bare init silently adopted CRD manifests as instance config. | #2513 | Fixed, PR #2539 |
| init | Help/man/reference promised a layout the scaffold never creates. | #2446 | Fixed, PR #2525 |
| Env | No local doctor: `goobers doctor` is k8s/forge-policy only; harness/auth/token/repo/sandbox checks are scattered across `validate --check-harness`/`--check-repos` (opt-in), daemon-startup preflight, and a Windows-only `preflight`. | #668, doctor.go:102 | §3 R2 |
| Env | Harness preflight: claude-code unusable on stock macOS (USER missing from procenv + envPassthrough ignored); probe stderr discarded so uninitialized Copilot is misreported as auth failure; Keychain-held Claude creds silently not seeded. | #2516, #2515, #2517, #2070 | §3 R2 |
| Connect | quickstart@v1 omits every remote-setup step its workflow needs; `your-org/your-repo` placeholders validate clean and die at the first live query. Token-name fork: seeding reads GOOBERS_GITHUB_ISSUES_TOKEN while the instance uses GOOBERS_GITHUB_TOKEN. | #2449 | §3 R3 |
| First PR | Starter default-implement's terminal stage promises "open a PR" with no push/PR stages or capabilities; quickstart-v1 has the exact working stage pair to copy. | #2447 | **Decision 7.3** |
| Docs | Demo-pause claim (#2452, fixed PR #2524); one-repo-per-instance advice stale since #667 (#2514, fixed PR #2523); reference gaggle's backlog label matches nothing ever (#2508, fixed PR #2526); curator taxonomy half-invalid (#2512, fixed PR #2526); releases.md claimed Windows doesn't compile (fixed PR #2524). | — | Fixed |

### 2.2 Structural causes (why it re-breaks)

- **Guards are self-referential.** Regen-diff CI guarantees man/help/completion
  match the registry, but nothing checks the registry against behavior (#2446)
  and the registry doesn't model flags at all — synopsis, completion specs,
  and handler FlagSets are four separately hand-synced authorities with ≥12
  verified drifts today (#2445, #2430).
- **Prose guides have zero executable checks** — #2447/#2448/#2449/#2452 all
  shipped through green CI. The only journey guard is release/docs.go's
  byte-exact section pinning: brittle (any wording edit breaks packaging,
  #2093) and partial (macOS/Windows quickstarts unpinned).
- **Validation is fail-open exactly where new users fall.** Placeholders pass;
  the gaggle-project↔instance-repos join is checked nowhere (a mistyped owner
  silently matches nothing at runtime, or silently binds to repos[0]);
  required-input checking covers 5 of ~40 stage commands; tool/skill refs
  warn only (#1470); Task.Inputs accepts any key.
- **Inert/hardcoded surface ledger** (fields that load and do nothing, stages
  that ignore config): BacklogRef.Query (#1677), allow-preview-features
  (#2046), select-source's hardcoded trust label (#2495), name-keyed telemetry
  rollups (#2494), merge-review's 9 hardcoded-GitHub sites (#2496, CONF-7/8/9
  in flight). The class recurs (#2490, #2493 fixed) because no lint forbids
  shipped-name/label literals in stage code.
- **instance.yaml is the biggest surface a new user authors (~140 fields) and
  the least supported**: no JSON Schema, no `goobers schema instance`, one
  prose-only INSTANCE001 diagnostic at path "/", validated by a cyclo-105
  monolith (#2027) — while the config/ tree gets stable codes, line/col, and
  did-you-mean.
- **No test composes the ladder.** Install is tested with a stub binary; the
  PR-producing e2e bypasses the daemon (in-process, package-var fakes); the
  daemon e2e uses the mock provider and opens no PR; init was tested only in
  empty temp dirs (why #2513 shipped green); native service lifecycle has
  zero real-supervisor coverage (#2438); the only real-agent test is
  quarantined advisory.
- **Agent-context assets can't reach a new user.** The toolkit install
  requires an already-valid config source (the assistant meant to help you
  author config demands authored config first); skills never land where
  Claude Code/Copilot natively discover them; the two most schema-facing
  files (dsl-reference.md, terminology.md) sit outside every digest pin —
  and drifted (failure-class evaluator missing; the claude-code harness,
  shipped 2026-07-25, absent from all 21 example goobers and every skill).
- **Four load-bearing design docs claim "not implemented" while fully
  shipped** (dsl-version-lifecycle, versioning-and-compatibility, workflow-cd,
  tutor-redesign) — a proven cause of duplicate scoping.

### 2.3 Shipped prior art this design builds on (do not re-design)

- **DSL version lifecycle** (dslVersion pins, SupportMatrix, coexisting
  interpreters, `goobers versions|fix|features`) — shipped; constrains all
  schema evolution here.
- **CLI-first onboarding action contract** (versioned JSON envelopes:
  `{action, version, created[], skipped[], path, nextCommand}`; portal/TUI are
  thin wrappers; every new rung is a registry command, not a script) — shipped
  and binding (docs/design/v1/cli-surface-and-manpages.md §5).
- **Quickstart ladder + guided init + demo + sample app + agent toolkit**
  (epic #435, ~90% closed) — extended, not replaced.
- **Validation engine** (single path for validate/lint/daemon preflight;
  stable codes; line/col; did-you-mean; path simulation; CLI-verb drift check
  #650) — the foundation §4 extends.
- **Drift-guard repertoire**: regen-diff goldens, config-PR validation gate,
  `goobers config diff`, shipped-workflow contract tests, hermetic test tier,
  schema enum-parity + description-coverage tests, CRD manifests now generated
  and CI-gated.
- **Stack neutrality mechanism** (per-gaggle ciCommand rewrite, fail-closed
  requiredCapabilities at load/schedule/stage-start, toolchain probers,
  default-deny env allowlist; Node/.NET/Java/Python reference gaggles) —
  shipped; §3 R4 wires it into the ladder's defaults.
- **Trust governance**: tutor allow-roots + D1-D6 rules govern any
  agent-authored config editing; SEC-048 (no phone-home) keeps all onboarding
  metrics journal-local (Time-to-First-PR is already instrumented, #1358).
- **e2e soak harness design** (#1479-81, accepted, unimplemented): its driver
  shape (container + init + `goobers up` + CLI-driven run + readservice
  observation, invalid-vs-fail contract) is the ladder gate's blueprint.

## 3. The ladder (design)

Every rung is a registry command returning the shipped onboarding JSON
envelope, so the CLI carries the whole path and the portal can wrap it later
(#437 stays decision-complete and untouched by this design). Every rung ends
by printing the next rung's command — and `goobers dashboard` for observation
— so discovery survives the primary path.

**R0 — install.** Real release artifacts (Decision 7.1) make the documented
install blocks executable; installer becomes install-only by default
(Decision 7.2). Until 7.1 lands, platform guides lead with build-from-source.

**R1 — init.** `goobers init [--guided] <path>` with the #2513 fail-closed
target checks (landed). Guided init's remaining sharp edge: a failure after
materialization dead-ends the target (first-run-only check refuses its own
partial output; the EventInitCompleted journal marker that distinguishes
"half-finished" from "real instance" already exists but is not consulted) —
wave B files the resume/replace fix. Guided also gains: detected-stack
`requiredCapabilities` emission (today it detects the stack for ciCommand but
forfeits the entire fail-closed scheduling mechanism), and an agent-toolkit
offer that actually reaches the config source (§4.4).

**R2 — validate the environment: flagless `goobers doctor`.** One command
aggregating what already exists behind scattered opt-ins: instance-root/layout
sanity, config validation, placeholder lint (§4.2), harness installed+signed-in
(reusing checkHarnessesAtSources — with #2515/#2516/#2517/#2070 fixed so the
probes tell the truth), token env presence per declared credential, repo
reachability (`--check-repos` engine), sandbox capability (Seatbelt/bubblewrap
per ADR-0001), and declared toolchain claims (toolchain.Verify). Modeled on the
WSL preflight's per-failure remediation text — every red line names the exact
fix command. `--k8s`/`--repo` remain as submodes. Doctor is advisory (exit
codes per finding severity); daemon startup keeps its own fail-closed preflight.

**R3 — connect your repository.** The missing choreography behind #2449, as
one command (working name: `goobers connect <owner>/<repo>`, non-interactive
flags + JSON envelope per the action contract): writes owner/repo/provider
into instance.yaml repos[] and the gaggle's project/backlog, sets the token
env-var name (no values, paste-guard as in guided), optionally seeds
labels/starter issue (reusing the idempotent stub-sample seeding — and closing
its GOOBERS_GITHUB_ISSUES_TOKEN vs GOOBERS_GITHUB_TOKEN fork), then runs the
R2 doctor checks scoped to that repo. The quickstart guide's §2 collapses to:
create-or-choose a repo, `goobers connect`, `goobers run`. Guided init reuses
the same core instead of its parallel prompt path.

**R4 — hello-world in your repo, then a real PR.** Two sub-rungs:
(a) *read-only proof*: the hello-world workflow (backlog-query --read-only +
the repo's real build command) — embedded into `goobers examples` and offered
by connect; proves credentials, backlog visibility, and the build gate with
zero writes. (b) *agentic PR*: the starter workflow gains deterministic
push-branch/open-pr stages and capabilities (Decision 7.3).

**Shipped since filing (#2549, PR #2583, landed 2026-08-07):** the quickstart
template's `local-ci` stage runs a real command
(`internal/instance/quickstart-v1/gaggles/example/workflows/quickstart.yaml`),
and the getting-started sample gaggle declares `ciCommand: ["npm", "run",
"ci"]` (`internal/instance/quickstart-v1/gaggles/example/gaggle.yaml`), which
`instance.ApplyGaggleCICommand` compiles into
that stage in place of its `make ci` default (MGV-1/#1009's pre-existing
mechanism). `cmd/goobers/gettingstarted_sample_test.go`'s
`assertGettingStartedLocalCI` asserts the stage actually executes and
inspects its artifacts for the sample's real `npm run ci` output — this is
tested, passing, end-to-end behavior for the Node path, not aspirational.
`samples/getting-started-task-api/sample.json`'s `localCI` field is a
separate, still-unread leftover (`onboardingSampleMetadata` in
`release/onboarding.go` has no field for it) — a small hygiene item, not
evidence that CI wiring is missing.

**Still open:** the wiring above only covers the Node/npm sample. Per-stack
samples — seed from test/e2e/testdata/{java,python,dotnet}service so
non-Node users don't need Node for the tutorial rung (Decision 7.5 covers
Swift, which has no prober today) — remain unbuilt; those stacks' fixtures
exist only under e2e test coverage today, not wired into `goobers examples`
or the connect/hello-world flow.

**R5 — customize with confidence.** `goobers agent-kit install` offered where
users actually are (guided completion, connect output, quickstart §6);
scaffold goober/workflow; `goobers examples` serving the full config-examples
tree (today 4 of 12 workflows embedded); the flagship chain (merge-review +
pr-remediation) documented as the graduation target with a stack-neutral
reference (today pr-remediation exists only in the Go self-host gaggle, so
non-Go operators hand-port the most complex workflow from Go-specific
prose). The rename-safety work (§4.5) makes customization survivable.

**Demo (side rung).** Retained as the credential-free tour for people who want
it; never the spine. No new product surface (PO ruling).

## 4. DSL authoring support (design)

### 4.1 instance.yaml joins the schema world

Author a full instance.schema.json (or generate it via reflection from
instance.Config), register it in schemas.Kind so `goobers schema instance` and
`goobers explain instance.*` work and editors complete the file new users
struggle with most. Fold into the #2027 decomposition: each section struct
owns its validate() and its schema fragment. Structured diagnostics (stable
codes, field paths) replace the single INSTANCE001 blob. Anticipate the MGV-14
credential-schema break (#1794-#1796, resolved-but-unimplemented, breaking):
generated examples and docs must regenerate through it, not hand-survive it.

### 4.2 Fail-closed where new users actually fall

- **Placeholder lint**: a dedicated validate code for `your-org/your-repo` and
  unedited template markers; run post-init in every mode ("next: edit these
  files"), red in doctor, warning in validate (error under --strict).
- **The join check**: every gaggle project/additionalRepos must match an
  instance repos[] entry (did-you-mean on owner/name); flag reliance on the
  silent single-repo empty-project fallback.
- **Task.Inputs**: extend the required-inputs registry across the full stage
  roster and warn on unknown input keys — emitted by cmd/goobers itself so the
  data cannot drift (the #650 pattern).
- **Tool/skill registries** (#1470): sourced from harness adapters and
  gooberassets; SKILL002 and a new TOOL001 become errors under --strict.
- **Inert-field ledger**: implement-or-remove BacklogRef.Query (#1677); thread
  trustLabel through select-source (#2495); preview gate per Decision 7.4;
  finish the CONF-7/8/9 provider-dispatch work (#2496 class).

### 4.3 One CLI authority + discoverable help

Attach structured flag/positional metadata to registry entries (#2445):
handler FlagSets constructed-or-checked from it, synopsis rendered from it,
completionFlagSpecs replaced by it, man pages gain a real OPTIONS section.
Interim (cheap, immediate): a reflection parity test diffing each handler's
real FlagSet against completion specs + synopsis — catches all 12 known drifts
without restructuring. Then: `goobers help <command|concept>` routes (unknown
topics error instead of silently printing core usage at exit 0), concept
topics rendered from prose shared with docs/concepts (which gains the missing
`instance`, `harness`, `capability`, `manifest`, `tier` entries), usage footer
gains quickstart/DSL-reference/troubleshooting pointers, and the authoring
commands (schema/explain/features/fix) become discoverable (core tier or an
"authoring" help group).

### 4.4 Agent-context pack (customization enabler, not requirement)

Extend the existing agent-toolkit — the release-matched grounding rule is
ratified; no parallel mechanism:

- **Bootstrap mode**: drop the valid-config-source precondition (or add
  `--bootstrap`) so the toolkit can land in an empty repo *before* first
  authoring; add an instance-root install target so init/connect can offer it.
- **Native discovery**: additionally materialize skills where harnesses
  actually look (.claude/skills/ for Claude Code; Copilot's equivalent),
  digest-tracked mirrors with .goobers/agent-toolkit canonical.
- **Truth guards**: extend the capability-presence test to evaluator kinds,
  harness constants (claude-code!), exact-set comparison; add dsl-reference.md
  + terminology.md to the test/dslauthor digest pin; add a relative-link
  integrity walk over the built bundle (the shipped bundle has a dangling
  goobers-io link today).
- **Generated field reference**: emit the trigger/task/gate field tables from
  api/schemas/*.schema.json + field-purposes.json (docsgen pattern), teaching
  the negative space explicitly (goobers-io auto-wiring has deliberately no
  YAML; contextFrom is a filter, not a wiring key) — the class a schema dump
  misses and hand-prose drifts on.
- **Machine-readable CLI surface**: the registry walk that renders man pages
  also emits a commands JSON artifact for agent consumption, pinned by the
  same regen-diff test.

### 4.5 Rename-safe customization

Introduce the workflow role/kind marker (#2494's ask; Decision 7.6) and key
telemetry rollups, remediation checks, and WorkflowBudgets off it. Add the
stage-code lint forbidding literal shipped workflow/label names (the
#2490/#2493/#2494/#2495/#2508 class) so "only the maintainer's hand-grown
instance works" stops being the steady state.

## 5. Docs that stay true

- Replace release/docs.go byte-pinning with marker-comment-delimited regions;
  extend adaptation to the macOS/Windows quickstarts; add a repo-wide
  markdown link+anchor checker to CI (would have caught the broken macOS
  anchor).
- Gate prose guides the way configvalidate gates config trees: extract and
  execute the quickstart's command blocks against a scratch instance in CI
  (or minimally assert named workflows/stages/files exist in shipped
  templates).
- Retire V0-ACCEPTANCE.md to an explicit historical banner once the §6 gate
  exists (it documents an obsolete third bootstrap method and is linked from
  the quickstart as current).
- Refresh the four stale design-doc status headers (PR filed with this
  design); adopt a generated shipped/remaining badge later if churn recurs.
- stack-support.md tier table gains a parity test (a "Shipped, green" row
  requires a shipped gaggle AND a CI-executed e2e leg); .NET/Python e2e legs
  get CI wiring or honest "validated locally" labels.

## 6. The full-ladder regression gate

`test/onboardingvalidate`, a Go-native gate job in the linuxvalidate mold
(real binary, evidence dir, own ci.yml job), composing existing seams:

1. **Install**: package the real binary via release.packageArchive + install.sh
   (the integration test does this with a stub today).
2. **Init**: `init --template=quickstart` non-interactively (+ a guided run
   with scripted stdin), including the populated-cwd refusal cases (landed
   with PR #2539).
3. **Doctor**: assert the R2 aggregate passes/fails as seeded.
4. **Connect**: promote fakeGitHubServer out of cmd/goobers' test package;
   requires the one missing product seam — a config/env-reachable BaseURL for
   the GitHub provider (GitHubProvider.BaseURL exists; Gitea/ADO already read
   config) — then `goobers connect` against the fake.
5. **Daemon + agentic PR**: real-process `goobers up`, run dispatched through
   live-daemon delegation, agent stage served by a stub harness binary
   (established re-exec pattern: writes ResultEnvelope completions, commits a
   diff) — asserting the PR, stage chain, and journal-local Time-to-First-PR.
6. **Stacks**: matrix the hello-world build gate over the per-stack sample
   fixtures (Go + Node in the gate; Java already CI-green; .NET/Python per
   §5). #2171 closes as the flagship-chain member of this family.

Hygiene preconditions before the gate becomes required: fix the demo-tour
flake (#1557 leaked global OTLP provider) and delete its wall-clock assert
(the repo's own never-assert-wall-clock lesson); service-lifecycle smoke
(#2438) rides systemd user-mode on ubuntu-latest separately. A manual-dispatch
real-agent variant (GHCP PAT exists in CI) mirrors ghcp-echo.yml — optional,
never a merge gate.

## 7. Decisions (recommendations adopted as rulings)

PO delegation 2026-08-07: "approved to self merge, i will not be reviewing any
of your stuff, if its quality and you need it, merge it." Accordingly the
recommendations below are adopted as the rulings of record; each still lists
its reasoning so a later human can revisit. Sequencing note for 7.1: the
first-ever release is cut only after 7.2 (installer rework) lands, so the
first published installer is never the guided-forcing one.

1. **Cut a v0.x pre-release.** The install rung is fiction until one tag
   exists; the entire machinery is release-tested. *Recommend: yes — tag a
   pre-release now, marked pre-GA; the packaging engine and #2039 smoke tests
   already gate it.* (PO action: approve; I can drive the release engine.)
2. **#2448 installer**: install-only by default, print demo + guided as next
   steps, `--guided` opt-in; release/docs.go's "already ran guided setup" text
   updated with it. *Recommend: yes; low urgency (no releases yet), lands with
   7.1.*
3. **#2447 starter first-PR gap**: add deterministic push-branch/open-pr
   stages + capabilities to starter default-implement (quickstart-v1's proven
   pair), keeping the agentic stage scoped to implementation. *Recommend: yes
   — the PO's stated hello-world outcome is a PR in your repo; the alternative
   (re-scoping docs to local-only) contradicts it.*
4. **#2046 preview gate**: resolved default-off. Generated manifests omit
   allow-preview-features, while configurations that deliberately use preview
   DSL must explicitly opt in.
5. **Swift/stretch stacks**: guided detection suggests `swift test` with no
   prober behind it. *Recommend: add the swift prober + Cargo.toml detection
   (small), defer Android (#742 stays the stretch).*
6. **#2494 role marker shape**: dedicated spec field vs annotation.
   *Recommend: optional `spec.role` enum (implementation|curation|remediation|
   review|custom) with name-based fallback for compatibility.*
7. **Staged mode (#1304/#1323) disposition** — PO asked for a fresh look
   2026-08-04. *Recommend: the ladder subsumes the safe-first-week story
   (read-only hello-world → single manual-trigger PR workflow → flagship
   chain), so re-scope #1323 onto the R3 connect choreography rather than a
   fifth init mode, and keep TBH-2 staged-lite (merge/close previews) as the
   Trust & Isolation milestone item it already is. No new staged surface in
   this design.*

## 8. Work breakdown

**Wave A — landed with this design (direct PRs, 2026-08-07):** #2513 (PR
#2539), #2446 (PR #2525), #2452 + releases.md + macOS anchor + hello-world
defaults (PR #2524), #2514 (PR #2523), #2508 + #2512 (PR #2526), stale design
headers (PR alongside this doc), #2549 quickstart local-ci stage wired to the
gaggle's declared `ciCommand` (PR #2583 — see R4 above; also most of #2171's
proof).

**Wave B — decision-free, filed 2026-08-07 (goobers:approved, assigned):**
- #2541 guided-init resume/replace for partial targets (EventInitCompleted).
- #2515/#2516/#2517/#2070 harness preflight truthing batch (approved+assigned
  as a unit).
- #2542 guided init emits requiredCapabilities + runner.capabilities from the
  detected stack; CI-detection against the target repo (or labeled as a
  cwd guess); Cargo.toml detection.
- #2543 placeholder lint + post-init validate in bare/demo/template modes.
- #2544 gaggle↔instance repo join check with did-you-mean.
- #2545 FlagSet↔completion↔synopsis reflection parity test (interim #2445
  guard, child of #2430).
- #2546 `goobers help <topic>` routing + glossary completions + usage footer
  pointers.
- #2547 markdown link/anchor checker in CI.
- #2548 agent-toolkit truth-guard batch (evaluator kinds, harness names,
  digest-pin extensions, link-integrity walk) + claude-code documented across
  assets.
- #1557 demo-tour flake fix + wall-clock assert removal (gate hygiene,
  approved+assigned).
- #2550 stack-support.md parity test; config-examples 'make ci' literal guard.
- #2551 stage-code hardcoded-name lint (#2490 class).

**Wave C — review-gated on §7 rulings:** release cut + installer rework
(7.1/7.2); starter PR stages (7.3); preview-gate removal (7.4); `goobers
doctor` local mode; `goobers connect`; instance.schema.json + #2027
decomposition; #2445 registry flag metadata (full); role marker (7.6);
per-stack samples; agent-kit bootstrap/native-discovery/generated reference;
prose-guide execution gate; release/docs.go marker regions.

**Wave D — the §6 gate**, sequenced after C's connect/doctor exist; GitHub
BaseURL seam and fakeGitHubServer promotion can start immediately.

Existing issues this design subsumes or re-scopes: #2431 (children resolved by
waves A/C), #2449→R3, #2447→7.3, #2448→7.2, #1323→7.7, #668→R2 submode kept,
#2171→§6.6, #153 (adjudicating its two competing PRs separately as watcher
work), #437 (untouched: still CLI-wrapped, still decision-complete).

## 9. Non-goals

- No terminology renames; no new init modes; no portal work (#437 proceeds
  independently); no mode-picker (#810 stays deferred); no containerized
  stages (#1494); no phone-home of any onboarding metric (SEC-048 —
  Time-to-First-PR stays journal-local); no staged-mode surface (7.7); no
  Homebrew/container packaging in this wave (#33).
