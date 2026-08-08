# Cold-start: ado

completed=True validate_clean=True tweaks=7 (validate 3, runtime 4), yaml_authored=557

## Summary
I got to a clean `goobers validate --strict --check-repos --check-harness` (exit 0, 0 errors, 0 warnings) with a full backlog-curation + implementation pair against ADO as both provider and backlog, in about 7 tweaks. The engine is genuinely provider-neutral where it counts: the three-part ADO repo identity is first-class in the schema, `--check-repos` did a real authenticated ADO git check, instance.yaml validates ADO auth kinds by name, and the gaggle/instance mismatch error rendered `freemasoninc/Goobers/goobers-testbed-ado` with a did-you-mean. The capability vocabulary is where ADO is a GitHub shadow: `goobers explain` advertises five `ado:*` grants that no shipped stage accepts, so every backlog stage on an Azure Boards backlog must declare `github:issues:write`, and `report-pr-status` — the one stage whose man page says "Azure DevOps PR status" — rejects `ado:pr:status`, demands `github:pr:write`, and explains itself in an error naming GitHub and Gitea but not ADO. The onboarding ladder simply has no ADO rung: `goobers connect` refuses the ADO identity outright and, worse, accepts the two-part guess and writes a `provider: github` config pointing at my ADO org. Two things I had to guess and the tool never checked: what `backlog.project` means for ADO (a bogus value passes strict validation with `--check-repos`), and which ADO surface `ci-poll` reads for pipelines. Verdict: ADO is a real, well-engineered provider with a good auth guide and an honest capability matrix, wrapped in an onboarding and example surface that assumes GitHub end to end — the plumbing is first-class, the on-ramp is not.

## Ledger

### 1. [docs-gap / via validate]
TWEAK 1 — replaced `ado:work-items:write` with `github:issues:write` on all 11 backlog-touching capability declarations (both workflows + the curator goober).
- expected: An ADO-backed backlog declares work-item writes with the ADO-namespaced capability. `ado:work-items:write` is in the enum that `goobers explain goober.spec.capabilities` prints, and ADR 0002 (docs/adr/0002-provider-neutral-capability-namespaces.md) states that provider-specific operations use `<provider>:<resource>:<verb>`.
- actual: 24 validation errors. Verbatim: `ERROR   gaggles/goobers-testbed/workflows/backlog-curation.yaml Workflow/backlog-curation: task "query-backlog" invokes built-in subcommand "backlog-query" but does not declare capability "github:issues:write"; the write capability-scoped credential is not injected, so backlog query and mutation operations fail at runtime` and `ERROR ... task "curate" policy action "label-issue" requires capability "github:issues:write", but the task does not declare it`. Every backlog built-in (backlog-query, backlog-health, backlog-dedupe, issue-close-out) demands the literal GitHub spelling regardless of provider. `ado:work-items:write` is accepted by the schema but is required by nothing — I later confirmed it validates clean under --strict as a dead, inert grant.

### 2. [docs-gap / via validate]
TWEAK 2 — changed `provider:pr:write` to `github:pr:write` on the `gather-implement-context` stage.
- expected: Per ADR 0002, stages that dispatch through the configured repository provider take the neutral `provider:pr:write`. `open-pr` and `ci-poll` do, so a PR-reading context-gathering stage should too.
- actual: `ERROR   gaggles/goobers-testbed/workflows/implementation.yaml Workflow/implementation: task "gather-implement-context" invokes built-in subcommand "gather-implement-context" but does not declare capability "github:pr:write"; the capability-scoped credential is not injected, so implementation context collection fails at runtime`. There is no published per-stage table of which built-ins are provider-neutral and which are still GitHub-pinned, so on ADO you find out one stage at a time.

### 3. [docs-gap / via validate]
TWEAK 3 — changed `ado:pr:status` to `github:pr:write` on the `report-status` stage.
- expected: `goobers report-pr-status` is the ONE stage in the whole product whose man page is explicitly Azure DevOps ("Publish a provider-native pull-request status (Azure DevOps PR status)"), so the ADO-named enum value `ado:pr:status` should admit it.
- actual: `ERROR   gaggles/goobers-testbed/workflows/implementation.yaml Workflow/implementation: task "report-status" invokes built-in subcommand "report-pr-status" but does not declare capability "github:pr:write"; the capability-scoped credential is not injected, so publishing the pull-request status fails at runtime (the GitHub PR-write grant also authorizes the Gitea commit-status path)`. The diagnostic on the ADO-native stage enumerates GitHub and Gitea and never mentions ADO. Sharpest single finding of the exercise.

### 4. [docs-gap / via docs-contradiction]
TWEAK 4 — changed implementation `excludeLabels` from `goobers:status:in-review` to `goobers/status:in-review`.
- expected: The in-review park marker follows the same colon convention as the two spellings the `goobers issue-close-out` man page does document (`goobers:needs-human`, `goobers:needs-remediation`).
- actual: No error — this fails silently and forever. The man page documents what status=needs-human and status=needs-remediation write but is silent on what status=in-review writes; only a code comment inside config-examples/gaggles/acme-web/workflows/implementation.yaml reveals the slash form. A mismatch means the exclusion never matches and the same work item is re-claimed on every scheduled tick. Nothing in validate, schema, or explain covers label-name contracts between two stages.

### 5. [default-assumption / via guessing]
TWEAK 5 — removed `labels: [goobers]` from the gaggle's backlog ref.
- expected: The `goobers init` scaffold and every config-example ship `labels: [goobers]` on the backlog ref, so keeping it is the documented default.
- actual: No error — but the candidate pool is provably empty. I queried the assigned ADO backlog myself (WIQL + work-items REST): all 5 User Stories carry tags like `bug; reporting` / `cli; enhancement`, none carries `goobers`. Combined with the mandatory `trustLabel: goobers:approved`, the default narrows to zero items. On GitHub `goobers connect --seed` "derives the labels the connected gaggles' backlog selectors actually require ... and files one safe starter issue carrying exactly those labels" — there is no ADO equivalent, so an ADO user has no supported way to seed the tag vocabulary and no signal that their selectors match nothing.

### 6. [default-assumption / via cli-help]
TWEAK 6 — hand-rewrote instance.yaml `repos[]` to the ADO three-part identity plus an `auth: {kind: pat}` block, deleted the scaffolded `example` gaggle, and hand-built the whole config/gaggles tree.
- expected: `goobers init` scaffolds a provider-agnostic starting point I can retarget.
- actual: `goobers init` hardcodes `provider: github`, `owner: your-org`, `name: your-repo`, `token.env: GOOBERS_GITHUB_TOKEN` in instance.yaml AND in the example gaggle's project + backlog, and its own post-init validation emits 4 warnings out of the box (2x SKILL002 for skills it scaffolded but did not create, 2x `WARNING PLACEHOLDER001 instance.yaml: contains unedited template marker(s) your-org, your-repo`). There is also no `instance` schema kind — `goobers schema instance` returns `error: unknown schema kind "instance"` and `goobers explain instance.repos` returns `error: unknown selector "instance.repos"` — so the one file that carries the ADO organization/project/auth-kind identity has no schema, no explain surface, and no reference page. I reconstructed it entirely from the YAML snippets in docs/guides/ado-authentication.md.

### 7. [default-assumption / via later-command-error]
TWEAK 7 — abandoned `goobers connect` entirely and edited YAML by hand.
- expected: Top-level help lists `goobers connect <owner>/<repo>` as THE way to "connect an instance to your own repository", so my first honest attempt was `goobers connect freemasoninc/Goobers/goobers-testbed-ado --token-env GOOBERS_ADO_TOKEN`.
- actual: Attempt 1 (correct ADO 3-part identity): `error: GitHub repository must be owner/name or a github.com URL (GitHub is the only supported provider in v1)`. Attempt 2 (2-part `freemasoninc/goobers-testbed-ado`) was ACCEPTED and rewrote instance.yaml and the gaggle to `provider: github` / `owner: freemasoninc` / `token.env: GOOBERS_ADO_TOKEN` — i.e. it wrote a permanently broken hybrid config to disk — then failed reachability against github.com with `REPOSITORY repos[0] freemasoninc/goobers-testbed-ado: unreachable: exit status 128: fatal: unable to get password from user`. The onboarding ladder's connect rung silently mis-provisions an ADO user who guesses the 2-part form.

## Delights
- `goobers explain --human goober.spec.capabilities` printed the complete allowed-value enum straight out of the build — that is the ONLY place in the entire product where the strings `ado:code:read`, `ado:pr:comment`, `ado:pr:write`, `ado:pr:status`, `ado:work-items:write` appear at all (zero hits across docs/guides, config-examples, reference-workflows, examples).
- The gaggle-to-instance repo cross-check is fully ADO-aware and renders the three-part identity with a did-you-mean: `ERROR Gaggle/goobers-testbed: spec.project repository freemasoninc/TotallyNotARealProject/goobers-testbed-ado matches no instance repos[] entry; did you mean "freemasoninc/Goobers/goobers-testbed-ado"?`
- `goobers validate --check-repos` performed a real authenticated ADO git reachability check with the PAT resolved from my env var and printed `REPOSITORY repos[0] freemasoninc/goobers-testbed-ado: reachable` — that single line confirmed org+project+repo+auth-kind+token wiring all at once.
- instance.yaml auth is validated with an ADO-specific message: `repos[0] (freemasoninc/goobers-testbed-ado): unsupported ADO auth kind "totally-bogus-kind"`.
- The missing-credential diagnostic names the exact variable: `unreachable: credentials: token ref "validate-repo-0": env var "GOOBERS_ADO_TOKEN" is not set`.
- Capability admission is genuinely fail-closed AND self-healing: every error names the exact capability string required plus the runtime consequence, so each fix was a one-line edit with no guessing.
- validate is batch, not one-error-at-a-time — it surfaced all 24 capability errors across both workflows in a single pass, so TWEAK 1 was one sed away.
- docs/guides/ado-authentication.md is a genuinely good, ADO-first document: four credential kinds, bounded-WIQL generation, process-state-category mapping so custom state names stay correct, Boards tags exposed as labels, revision-tested claim patches, and a separate ADO git-transport quota ledger. It reads like someone who actually uses ADO wrote it.
- docs/provider-capability-matrix.md marks ADO gaps with live issue numbers, and I used it to make a real design decision: `backlog.blockers` is `gap (#2059)` for ADO, so I deliberately omitted the GitHub reference's blocked-sibling re-sweep from my curation workflow and documented why in the YAML.
- `goobers connect`'s refusal is honest and unambiguous rather than a stack trace: `GitHub is the only supported provider in v1`.
- `goobers workflow show <name>` rendered both DAGs including every gate branch target, so I could eyeball the routing of an 12-stage workflow without running anything.

## Docs notes
The ADO story is two excellent documents plus a hole where the rest should be.

What exists and is good: docs/guides/ado-authentication.md (credential kinds, Boards/WIQL semantics, tags-as-labels, transport quota), docs/provider-capability-matrix.md (ADO in the "blessed tier" with per-capability conformance and gap issue numbers), and ADR 0002 on capability namespaces.

What does not exist anywhere: (1) a single ADO example. Zero YAML files under config-examples/, reference-workflows/, or examples/ use `provider: ado` — all 25 gaggle/workflow examples are GitHub. (2) An ADO onboarding path. docs/guides/arbitrary-repo-onboarding.md, the production-oriented guide, is titled "takes a GitHub repository" and mentions ADO exactly zero times; its token table is a GitHub fine-grained-PAT permission grid with no ADO PAT scope equivalent. quickstart.md §3 describes guided init as selecting a "target GitHub application repository". (3) Any documentation of the five `ado:*` capabilities — they are discoverable only via `goobers explain`. (4) Any mention of `goobers report-pr-status`, the single most ADO-native stage in the product (publish Goobers' verdict as an ADO PR status so a branch policy can gate on it). It appears in no guide, no example, no reference workflow; I found it only by listing docs/man/*.1 by hand. That is ADO's best feature and it is invisible.

Two docs statements I could not resolve from docs+CLI alone and had to guess: (a) what `backlog.project` should contain for ADO. The schema says "GitHub owner/repository or Azure DevOps project that scopes the backlog" — bare project name, no organization field exists on a backlog ref — so an ADO backlog silently inherits its organization from instance.yaml `repos[]` and a backlog in a different ADO org than the code repo cannot be expressed. (b) which ADO surface `ci-poll` actually reads for "ADO pipelines as CI" — pipeline runs, PR statuses, or branch-policy evaluations. Nothing says.

Validation gap worth fixing: the ADO backlog ref is never checked by anything. I set `project: freemasoninc/TotallyBogus` and `goobers validate --strict --check-repos` returned `OK` with exit 0, while the sibling repo ref gets a did-you-mean cross-check. The single most ADO-ambiguous field in the config is the one field validate has no opinion about.

Cosmetic inconsistency: `--check-repos` prints the ADO repo as the two-part `freemasoninc/goobers-testbed-ado` (dropping the project), while the gaggle-mismatch error correctly prints the three-part `freemasoninc/Goobers/goobers-testbed-ado`.

## DSL ceremony notes
The DSL itself held up well on ADO — it is the capability vocabulary, not the structure, that leaks GitHub.

Real ceremony: every backlog-touching stage must repeat `capabilities: [github:issues:write]` plus a `policyActions:` list whose entries each independently require that same capability, so the compiler makes you state the same authority twice per stage in two vocabularies. Across my two workflows that is 11 capability declarations and 14 policy-action entries that are all mechanically derivable from the built-in subcommand named in `run.command`. The validator already knows the mapping — it tells you the exact missing string — so it could default it and let the author narrow rather than making the author guess and get corrected.

The `ado:*` capability names are worse than ceremony, they are a trap: five ADO-named grants exist in the enum, none is required by any shipped stage, and declaring one extra alongside the real grant passes `--strict` silently. So the DSL offers an ADO author exactly the wrong word, rejects it on every stage, and then accepts it as dead weight if they leave it in.

Three ADO-shaped things the DSL genuinely got right: the neutral `provider:pr:write` spelling means `open-pr` and `ci-poll` needed no ADO-specific edits at all; `repoRef` carries a first-class optional `project` field with the description "Azure DevOps project. Omit for GitHub repositories."; and per-gaggle `ciCommand` + `requiredCapabilities` meant retargeting the Go-default reference workflow at a Python CLI was a two-line gaggle change, not a workflow fork.

One unavoidable hand-write: `ci-poll` needs a placeholder `run.command: ["goobers", "ci-poll"]` that is never executed (the runner dispatches on `inputs.kind: "ci-poll"`) purely because `DeterministicRun.Command` is schema-required. The example file admits this in a comment.
