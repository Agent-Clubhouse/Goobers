# Cold-start: minimal

completed=True validate_clean=True tweaks=5 (validate 4, runtime 1), yaml_authored=30

## Summary
The on-ramp is genuinely excellent and the back half is genuinely against the grain. `goobers init` self-validated and told me exactly which two files to edit; `goobers connect Agent-Clubhouse/goobers-testbed-minimal` then rewrote every placeholder across instance.yaml and gaggle.yaml, stored the credential by env-var name only, and live-checked reachability — I hand-edited nothing to wire up the repo, and the scaffold already shipped 4 of my 6 stages (query-backlog -> implement -> push-branch -> open-pr). Everything after 'open a PR' was uphill: the product's own requirements say merging belongs to a separate `merge-review` workflow, so carrying one workflow through to a merge meant bolting on `ci-poll` (an undocumented stage kind whose only authoring reference is a YAML comment in config-examples) plus a `pr-select` stage whose sole purpose in my loop is to re-discover the PR the run itself just opened, in order to feed `merge-pr` SHA-pins that nothing upstream emits. The best thing about the hour: `goobers validate` caught all four config tweaks statically, with messages naming the exact missing token and the exact runtime consequence — I never once had to run the daemon to learn I was wrong. The worst thing is what validate does not catch: `connect` said 'reachable' and `validate --check-repos` said OK, yet the loop would have claimed zero work forever, because the scaffold's `trustLabel: "goobers"` matches none of the target repo's five unlabeled issues and nothing at author time compares the two. Final state: 234 lines of YAML across 5 files (142 of it the workflow, ~30 lines truly hand-authored rather than transcribed from config-examples), `validate` exit 0 with one unavoidable warning — VER003 'no schedule trigger', which fires on exactly the manual-only shape the assignment asked for and that the scaffold itself ships by design.

## Ledger

### 1. [dsl-ceremony / via validate]
TWEAK 1 — added a `timeout` branch to the `ci-gate` automated gate (routed to `@escalate`).
- expected: A boolean CI gate only needs `pass` and `fail` branches; anything else should fall through to a sane default.
- actual: ERROR   gaggles/example/workflows/default-implement.yaml Workflow/default-implement: gate "ci-gate": producible outcome "timeout" has no branch (would fail closed at evaluation time)

### 2. [dsl-ceremony / via validate]
TWEAK 2 — inserted an extra `select-own-pr` stage (`goobers pr-select`) between the CI gate and `merge-pr`, purely to obtain `headSha`/`baseSha`, and rewired merge-pr's inputsFrom from `prNumber` to `number`/`headSha`/`baseSha`.
- expected: `open-pr` just opened the PR and `push-branch` just pushed the commit, so the run already knows its own PR number and head SHA; `merge-pr` should be able to take the PR number alone.
- actual: ERROR   gaggles/example/workflows/default-implement.yaml Workflow/default-implement: task "merge-pr" runs `goobers merge-pr`, which requires inputs [baseSha headSha], but the stage wires them through neither inputs nor inputsFrom; the command reads them via providerInput and exits non-zero when empty, so this stage fails every run

### 3. [docs-gap / via validate]
TWEAK 3 — added `policyActions: [flag-foundation-coupling]` to the `select-own-pr` stage.
- expected: `goobers pr-select --help` and `docs/man/goobers-pr-select.1` document the stage's inputs and outputs completely, so the declared `capabilities: [github:pr:write]` should be enough.
- actual: ERROR   gaggles/example/workflows/default-implement.yaml Workflow/default-implement: task "select-own-pr" command "goobers pr-select" prescribes policy action "flag-foundation-coupling" but policyActions does not declare it

### 4. [default-assumption / via validate]
TWEAK 4 — deleted `spec.skills: [implement, run-tests]` from the goober that `goobers init` itself scaffolded.
- expected: A freshly scaffolded instance validates warning-free before I touch it.
- actual: WARNING SKILL002 gaggles/example/goobers/coder/goober.yaml Goober/coder: spec.skills declares "implement", but no skill package directory was found at "gaggles/example/skills/implement" or "skills/implement" (same again for "run-tests") — emitted by init's own post-init validation, on init's own output

### 5. [default-assumption / via guessing]
TWEAK 5 — repo-side change required (`goobers connect --seed`, or hand-labelling the issues): the loop's `trustLabel: "goobers"` matches none of the target repo's 5 open issues, which carry no labels at all. No config-side fix exists because `backlog-query --claim` fails closed without a trustLabel (SEC-047).
- expected: `goobers connect <owner>/<repo>` reported "connected" and `validate --check-repos` reported "reachable", so I assumed the loop would find the repo's 5 open issues.
- actual: All 5 issues return `"labels":[]`. `goobers validate --check-repos --check-harness` exits 0 with no finding — nothing at author time compares the configured trustLabel/backlog labels against the repository's actual labels or eligible-item count. The loop would have started, claimed nothing, and finished as a silent no-work run.

## Delights
- `goobers init` ran its own post-init validation automatically and ended with a literal 'Next: edit these files before running a live workflow:' list naming the exact two files that still held placeholders. I never had to ask what to do next.
- `goobers connect Agent-Clubhouse/goobers-testbed-minimal` rewrote every your-org/your-repo placeholder across instance.yaml AND config/gaggles/example/gaggle.yaml in one command, recorded the credential by env-var NAME only (help says it rejects a pasted token value outright), and live-checked repo reachability using the exact credential path a real run would use. Zero hand-editing for repo wiring.
- `validate` caught the missing merge-pr SHA-pins statically and explained the consequence inline ('the command reads them via providerInput and exits non-zero when empty, so this stage fails every run'). That is a guaranteed runtime failure caught at author time, without ever running the daemon.
- Every task/gate error named the exact missing token — the outcome `timeout`, the policy-action string `flag-foundation-coupling`, the input list `[baseSha headSha]` — so each fix was a copy-paste rather than a hunt.
- `goobers workflow show default-implement` rendered the whole compiled state machine including gate branch targets (`pass -> select-own-pr`, `fail -> @abort`, `timeout -> @escalate`), letting me eyeball the loop without running it.
- `validate --check-harness --check-repos` verified the copilot harness and repo reachability without starting the daemon, and `validate --json` emits a clean versioned diagnostics envelope with error/warning counts and a `path` pointer per finding.
- `docs/guides/github-token-scopes.md` proactively documents the exact trap I would otherwise have hit blind: on a private repo with a fine-grained PAT, `ci-poll` cannot read check-runs unless you also grant Actions: Read. Being warned about a failure before hitting it is rare.
- The scaffolded starter workflow is manual-only *by design*, with an in-file comment stating why ('so a fresh install never starts provider work without an operator action'). Safe default plus its reason, in the file.
- `goobers --help` reads like a map rather than a dump: it advertises `connect`, `examples`, `schema`, `scaffold`, `workflow show`, and documents the exit-code vocabulary (0/1/2, plus 3 = escalated) in the footer.

## Docs notes
The biggest gap is `ci-poll`. It is a first-class built-in stage kind, listed GA in docs/feature-matrix.md, and central to the flagship shipped `implementation` example — but it has no command surface at all: `goobers help stages` lists ~35 stages and omits it, `goobers ci-poll --help` prints `goobers: unknown command "ci-poll"` and dumps the top-level usage, there is no docs/man/goobers-ci-poll.1, and docs/cli/README.md mentions it only obliquely inside the *merge-queue-poll* entry ('default to internal/executor's ci-poll defaults'). Its entire authoring contract — that you set `inputs.kind: "ci-poll"`, that `run.command` is a required-field placeholder that never executes, that it needs `prNumber` via `inputsFrom`, that it emits `ciStatus` — exists only as YAML comments inside config-examples/gaggles/acme-web/workflows/implementation.yaml. Delete that one example file and `ci-poll` becomes unauthorable from shipped docs.

Second: nothing documents which stages *produce* `headSha`/`baseSha`. `merge-pr --help` says they are required and that `verdictAuthor` is 'supplied by apply-verdict', but never says where the SHAs come from. I had to grep config-examples/ to discover `pr-select` is the sole producer.

Third: `pr-select`'s mandatory `policyActions: [flag-foundation-coupling]` appears in neither its `--help` nor its man page — only in the merge-review example and in the validator's error text. The same shape of gap probably exists on other stages.

Fourth, and most relevant here: docs/requirements/pr-lifecycle.md is explicit that merging is architecturally owned by a *separate* `merge-review` workflow that starts from `pr-select` over the whole open-PR set. There is no documented pattern anywhere for 'implement and merge in one workflow' — which is exactly the degenerate loop. The docs' ordered onboarding path (demo -> `--template=quickstart` -> `init --guided`) stops at 'opens a pull request', and bare `init` is explicitly labeled 'Manual/advanced alternative'. So the simplest end-to-end loop a new user would ask for is the one the documentation has no route to.

Minor: init's own validation output mixes path prefixes — `config/gaggles/example/gaggle.yaml` in one warning and `gaggles/example/goobers/coder/goober.yaml` (no `config/`) in the next.

## DSL ceremony notes
The simplest expressible loop cost 234 lines of YAML across 5 files (instance.yaml 21, config/manifest.yaml 22, gaggle.yaml 22, goober.yaml 27, workflow 142). `goobers init` + `goobers connect` gave me 4 of those files essentially free; the workflow is where the ceremony lives.

Ceremony a smart default should have absorbed:

1. THE SHA-PIN ROUND TRIP. The run pushed the branch (`push-branch`) and opened the PR (`open-pr`) two stages earlier — it demonstrably knows its own head SHA and base branch. Yet `merge-pr` demands `headSha`/`baseSha` as explicitly wired inputs, and the only stage that emits them is `pr-select`, whose own help describes it as selecting a PR 'for merge-review'. So the degenerate loop must re-discover its own PR by branch-prefix search to learn facts it just created. That is 18 lines of pure plumbing, and it introduces a real correctness hazard: `pr-select` picks 'at most one open, non-draft, green-CI PR' matching `headPrefixes: goobers/default-implement/`, so a leftover unmerged PR from an earlier run of the same workflow could be merged instead of this run's. An `open-pr` that emitted `headSha`/`baseSha` alongside `prNumber` (or a `merge-pr` that resolved the pins from `pullNumber`) would delete the entire stage.

2. `verdict: "pass"` AS A LITERAL. My loop has no reviewer by assignment, so I must hand the merge stage a hardcoded string asserting a review that never happened. The stage is built around `apply-verdict` supplying it; the no-review case is expressed by lying to the conjunct.

3. `command: ["goobers", "ci-poll"]` FOR A COMMAND THAT DOES NOT EXIST. `DeterministicRun.Command` is schema-required, but for `kind: ci-poll` the executor never shells out — so the author must write an unrunnable command line as a placeholder. `goobers ci-poll` literally errors 'unknown command' if you try it.

4. `policyActions` RESTATING WHAT THE TOOL ALREADY KNOWS. The validator knows `goobers pr-select` prescribes `flag-foundation-coupling` — it told me so by name. Requiring me to type it back is an acknowledgment checkbox, not information. (Defensible as an explicit-consent design; still ceremony for the smallest loop.)

5. EXHAUSTIVE GATE OUTCOMES WITH NO DEFAULT ROUTE. `ci-status` produces `timeout`, so `pass`/`fail` is rejected outright. Sensible for a production loop; heavy for a two-branch one.

6. `expectedOutputs` RE-DECLARATION. Threading a value across stages means re-listing it in each stage's `expectedOutputs` and each consumer's `inputsFrom`; the shipped merge-review example threads `selectedNumber` through four stages this way.

Also: the shipped starter is manual-only by design, yet `validate` warns VER003 'has no schedule trigger' on it — so a manually-triggered workflow (the assigned pattern) can never pass `validate --strict`. My final config is clean on plain `validate` (ok: true, 0 errors, 1 warning) and that lone warning is structurally unavoidable for this pattern.
