# Cold-start: swift

completed=True validate_clean=True tweaks=10 (validate 2, runtime 8), yaml_authored=275

## Summary
I got to a clean `goobers validate --strict --check-repos --check-harness` (exit 0, zero warnings) with a full local-gate-only loop: query-backlog -> implement -> agentic review gate -> `swift build` -> build-gate -> `swift test` -> local-gate -> push-branch -> open-pr -> close-out, with bounded repasses back to the implementer and separate escalation parking. The first-hour experience is genuinely good: `init` self-validates and tells you exactly what to edit, `connect` rewired the whole config to my repo and live-checked reachability with token-by-name-only hygiene, `scaffold` inferred my hand-renamed gaggle, and `workflow show` let me verify the DAG without running anything. My config validated clean on the first full attempt, but only because I read four docs and three reference gaggles first — validate caught just 2 of my 10 tweaks, and both were cosmetic (a scaffolder emitting skill names it doesn't scaffold, and a missing schedule trigger). The eight substantive tweaks all came from docs contradictions, guessing, or CLI spelunking. On the assigned question: the machinery handles no-hosted-CI perfectly well, but nothing acknowledges the case — the canonical arbitrary-repo-onboarding guide lists "GitHub CI on pull requests, so the implementation workflow's `ci-poll` stage can reach a passing or failing state" as a hard prerequisite, the production workflow quickstart sends you to hard-wires ci-poll/ci-gate/remediate-ci, and the one shipped local-gate-only example can't open a PR, so I had to splice two references by hand. Most concerning: I built a probe with a full ci-poll/ci-gate workflow aimed at this zero-CI repo and validate called it OK even under `--check-repos`, which contacts the repository — as it also did for a capability the runner doesn't claim, a nonsense toolchain token, and gate branches made unreachable by a missing continueOnError. A new user following the recommended path lands a config that validates perfectly and then stalls on every single run.

## Ledger

### 1. [dsl-ceremony / via cli-help]
Hand-renamed the scaffolded gaggle from `example` to `swift-cli`: directory move, plus gaggle.yaml metadata.name, isolation.namespace, and config/manifest.yaml's gaggles list + metadata.name + spec.instance.name.
- expected: A way to name my gaggle at init or connect time, or a `goobers scaffold gaggle` to match the existing `scaffold goober|workflow`.
- actual: `Usage: goobers scaffold goober [--force] <name> [path] / goobers scaffold workflow ...` — no gaggle subcommand. `init` hardcodes the name `example` and `connect` rewrites repo coordinates but not gaggle identity, so renaming meant editing 5 fields across 2 files plus a directory move, by hand.

### 2. [dsl-ceremony / via docs-contradiction]
Split the local quality gate into two deterministic stages — `local-build` running `swift build` and `local-ci` running `swift test` — instead of expressing it in one gaggle-level field.
- expected: `ciCommand` would carry my whole local gate, since it is documented as 'the argv the local-ci stage runs for this gaggle' and is billed as one of the two fields a bring-your-own stack declares.
- actual: ciCommand's schema is a single argv (`"type": "array", "items": {"type": "string", "minLength": 1}`) — one command, not a sequence. My assigned gate is `swift build` AND `swift test`. Only the stage literally named `local-ci` receives the override, so `local-build`'s command is invisible to ciCommand and the gaggle-level declaration now honestly describes only half of this gaggle's gate.

### 3. [docs-gap / via docs-contradiction]
Added `continueOnError: true` to both `local-build` and `local-ci`.
- expected: Copying the shipped reference shape (`local-ci` -> `local-gate` with `fail: implement`) would give me a working failed-build-repasses-to-the-implementer loop.
- actual: The workflow schema says continueOnError controls 'Whether a failed result remains visible to the next gate instead of immediately failing the run.' Without it, the gate's `fail:` branch is unreachable and the whole repass loop is dead code. Two shipped references (config-examples acme-web/implementation.yaml and python-service/python-implementation.yaml) omit it on `local-ci` while a third (examples/ios-simulator) sets it before its gate. I removed it in a probe copy and validate still printed OK — nothing flags the unreachable branch.

### 4. [default-assumption / via validate]
Deleted the `skills:` block from both goober.yaml files.
- expected: Freshly scaffolded output validates warning-free.
- actual: `goobers init` printed 'WARNING SKILL002 gaggles/example/goobers/coder/goober.yaml Goober/coder: spec.skills declares "implement", but no skill package directory was found at "gaggles/example/skills/implement" or "skills/implement"' and the same for "run-tests". `goobers scaffold goober swift-implementer` then generated `skills: ["swift-implementer"]`, which warns identically. Both scaffolders emit skill names they do not scaffold packages for.

### 5. [default-assumption / via validate]
Changed the workflow trigger from the scaffolded `manual` to `schedule: "11,41 * * * *"`.
- expected: The scaffolded manual trigger is a reasonable starting point for an implementation loop.
- actual: 'WARNING Workflow/default-implement: workflow "default-implement" has no schedule trigger; it will not fire autonomously — run it with `goobers run default-implement`'. It is also either/or, not additive: the schema enforces that a manual trigger must be the only trigger.

### 6. [docs-gap / via cli-help]
Hand-wrote a `runner:` block in instance.yaml declaring capabilities os=darwin, swift@6.2, xcode.
- expected: `goobers schema instance` would give me the field reference for the one file `init` explicitly tells me to edit first.
- actual: `error: unknown schema kind "instance"`. There is no `instance` kind among the 24 published schemas even though gaggle, goober, workflow and manifest all have one. The only reference is a commented-out block in reference-workflows/instance.yaml.example. `init` writes `runner: {}` and nothing warns that a gaggle's requiredCapabilities must be *claimed* here or the run never schedules.

### 7. [capability-expectation / via guessing]
Added `runner.envPassthrough: [DEVELOPER_DIR, SDKROOT, TOOLCHAINS, MACOSX_DEPLOYMENT_TARGET]`.
- expected: The daemon's env allowlist would carry whatever the Swift toolchain needs, the way quickstart.md promises it passes the whole Go toolchain env family into every stage automatically.
- actual: instance.yaml.example states the built-in default-deny allowlist 'already covers the Go/.NET/Python/Node/Rust families' — Swift/Xcode is not among them. No shipped doc lists which variables a Swift or Xcode stage actually needs, so I had to guess the set from knowledge of SwiftPM rather than from the product.

### 8. [default-assumption / via guessing]
Set `runner.defaultStageTimeout: 25m`.
- expected: The default deterministic-stage timeout would accommodate an ordinary build/test entrypoint.
- actual: The built-in default is 10 minutes and instance.yaml.example warns it is 'sized for short commands — raise it if your build/test entrypoint routinely runs longer'. I raised it preemptively for a cold `swift build`. I then cloned the repo and measured: it builds in 3.5s and tests in 0.006s, so for this repo the raise was unnecessary. Nothing in the tool could have told me either way before an actual run — I was guessing in both directions.

### 9. [docs-gap / via docs-contradiction]
Removed `ci-poll`, `ci-gate`, and the `remediate-ci` agentic consumer from the reference implementation workflow, and re-routed open-pr -> open-pr-gate -> close-out so the local build/test gates are the only quality signal.
- expected: A documented local-gate-only variant of the production implementation loop, given my repo reports no check runs at all.
- actual: quickstart.md points you at config-examples/gaggles/acme-web/workflows/implementation.yaml, which hard-wires ci-poll + ci-gate + remediate-ci. docs/guides/arbitrary-repo-onboarding.md lists as a hard repository prerequisite: 'GitHub CI on pull requests, so the implementation workflow's `ci-poll` stage can reach a passing or failing state.' No shipped doc describes a no-hosted-CI path. The only local-gate-only example (python-service) omits the entire PR lifecycle — no push-branch, open-pr or close-out — so neither reference works as-is and I had to splice the two by hand.

### 10. [docs-gap / via guessing]
Kept `trustLabel: goobers` and backlog `labels: [goobers]` knowing the loop will claim nothing until that label exists; declined `connect --seed` rather than file a starter issue into a shared testbed.
- expected: `goobers connect`, which had just authenticated against this exact repo, or `validate --check-repos`, which contacts it again, would tell me my backlog selector matches nothing on the target.
- actual: Both report reachability only ('REPOSITORY repos[0] Agent-Clubhouse/goobers-testbed-swift: reachable'). I found out by running `gh label list` myself: the repo carries none of the goobers labels and all 5 open issues are unlabeled, so a scheduled run claims zero items forever and is indistinguishable from an idle daemon.

## Delights
- `goobers init` self-validated immediately and printed a precise 'Next: edit these files before running a live workflow' list naming the exact two files — no hunting.
- PLACEHOLDER001 caught the unedited `your-org`/`your-repo` markers in both instance.yaml and gaggle.yaml. A genuinely thoughtful check that a schema-only validator would never do.
- `goobers connect Agent-Clubhouse/goobers-testbed-swift --token-env GH_TOKEN` rewrote every placeholder across instance.yaml and gaggle.yaml, recorded the credential by NAME only, and did a live reachability check using 'the exact credential path a real run would use'. The refusal to accept a pasted token value is exactly the right instinct.
- `goobers scaffold goober` and `scaffold workflow` auto-detected the gaggle I had just renamed by hand — no --gaggle flag needed — and emitted skeletons that validate immediately.
- docs/guides/stack-support.md has an explicit 'Anything else | Bring-your-own' row that names the exact two fields (ciCommand + requiredCapabilities) my unsupported stack needs. It also states honestly which stacks are proven-green vs merely planned. I never had to wonder whether Swift was viable.
- reference-workflows/instance.yaml.example is the single best document in the tree: every block commented with why, including the proactive warning that the 10-minute stage default is 'sized for short commands' and the note that the env allowlist covers Go/.NET/Python/Node/Rust — which is exactly what told me Swift was on my own.
- `goobers workflow show` printed the whole DAG with every gate's branch targets, letting me verify the loop shape (including that fail branches route back to `implement`) without running anything.
- `validate --check-harness` and `--check-repos` gave real answers ('HARNESS copilot: OK', repo reachable) rather than static parsing — actual liveness checks at authoring time.
- The python-service polyglot reference is a real local-gate-only shape (implement -> review -> local-ci -> local-gate, zero check polling), which was concrete proof the DSL fully supports my assigned pattern even though no guide describes it.
- Consistent, uncompromising credential hygiene everywhere: every doc, schema and command insists tokens are references (env/file/store), never values, and says so with requirement IDs (CFG-009, SEC-010) I could grep.

## Docs notes
Direct answer to the assigned question: the MACHINERY handles no-hosted-CI cleanly, but the GUIDANCE assumes CI exists, and VALIDATE is completely blind to the difference.

Docs that assume hosted CI: docs/guides/arbitrary-repo-onboarding.md — the canonical "onboard your own repo" guide — states verbatim under "The target repository needs:" the bullet "GitHub CI on pull requests, so the implementation workflow's `ci-poll` stage can reach a passing or failing state." That is presented as a hard prerequisite with no alternative offered. Its walkthrough (steps 309-312) likewise bakes in "Polls GitHub CI and repasses on a real failure" and "marks goobers/status:in-review after CI". quickstart.md §2 tells you to graduate to config-examples/gaggles/acme-web/workflows/implementation.yaml for production shape, and that workflow hard-wires ci-poll, ci-gate, and a whole extra agentic `remediate-ci` goober-consumer that exists only to digest provider-authored check evidence. docs/guides/github-token-scopes.md-adjacent tables list Checks and Commit statuses read-only as expected grants.

Docs that DO acknowledge the case: only obliquely. docs/guides/stack-support.md is excellent on stack-neutrality (bring-your-own via ciCommand + requiredCapabilities) but that is about LANGUAGE, not about CI topology — a different axis that the docs never separate. The one shipped artifact with my actual shape is config-examples/gaggles/python-service/workflows/python-implementation.yaml (implement -> review -> local-ci -> local-gate, no polling anywhere), but it is marked "Planned", it never explains that it is also the no-CI shape, and it deliberately omits the entire PR lifecycle ("This focused polyglot reference omits stack-agnostic PR lifecycle stages"). So the local-gate-only example cannot open a PR and the PR-capable example cannot run without CI. I had to splice them.

A grep across docs/, README.md, config-examples/, examples/ and reference-workflows/ for "no CI", "without CI", "no checks", "zero checks" returns zero hits describing a repository that has no hosted CI. There is no man page for ci-poll either (it is dispatched via inputs.kind, not a real subcommand), so nothing documents what ci-poll does when a repo reports no check runs at all.

What validate does not catch (I probed each in throwaway copies; all four printed "OK: instance.yaml valid; config/ valid"): (1) a full ci-poll + ci-gate workflow pointed at this zero-CI repo — clean even under `--check-repos --strict`, which actually contacts the repository; the real-world outcome is every run parking at ci-gate's timeout branch. (2) a gaggle requiring swift@9.9 when the instance's own runner claims swift@6.2 — a guaranteed schedule-time deadlock that the same validate run could cross-check, since it reads both files. (3) a nonsense capability token `totally-made-up-toolchain@42` — runnerCapability is pure regex (^[A-Za-z0-9][A-Za-z0-9._@=+-]*$), so a typo like `swfit@6.2` is undetectable. (4) gate `fail:` branches made unreachable by removing continueOnError.

The cheapest high-value fixes: have `connect`/`--check-repos` report the target's check-run and label reality ("this repo has no CI checks and none of your selector labels"), and have validate cross-check gaggle requiredCapabilities against runner.capabilities.

## DSL ceremony notes
The DSL itself is expressive and the state machine is genuinely nice to author — `workflow show` proving the DAG without a run is a real strength. The ceremony is concentrated in a few places:

1. ciCommand is one argv. The field billed as "your stack's local CI command" cannot express a two-command gate (`swift build` then `swift test`), which is the most ordinary shape imaginable. Worse, it only overrides the stage literally named `local-ci`, so my second gate stage (`local-build`) is silently outside the gaggle-level declaration — the gaggle now under-describes its own gate, and a reader of gaggle.yaml alone would think `swift test` is the whole story. A list-of-argvs, or a documented convention for multi-command gates, would remove the split entirely.

2. continueOnError is load-bearing but invisible. Whether a deterministic stage's failure reaches its gate — i.e. whether your entire repass loop exists — hinges on one optional boolean that two of three shipped references omit. A gate declaring a `fail:` branch is a complete statement of intent; the runner should either infer continueOnError from it or validate should refuse the unreachable branch.

3. No `scaffold gaggle`. Renaming the one gaggle `init` gives you is a 5-field, 2-file, 1-directory-move manual edit, in a product where everything else is scaffolded.

4. Capability tokens are stringly-typed on both sides. `requiredCapabilities` and `runner.capabilities` are free-form regex-checked strings that must match each other exactly, with no shared vocabulary, no known-family warning, and no cross-check between the gaggle and the instance that declares the runner — despite validate reading both in the same pass. The docs even enumerate which families have probers (dotnet, node, python, go, java, os=) versus which are schedule-time only (xcode), which is exactly the knowledge a warning could encode.

5. instance.yaml has no published schema. `goobers schema instance` errors, so the one file `init` tells you to edit first is the only object you cannot introspect. `goobers explain <selector>` exists for schema field facts but has nothing to project for Instance.

Counter-note on ceremony: the workflow is 229 lines, but very little of that is redundant — the park-escalated / park-needs-human split, the failure-class vs status-equals distinction, and the open-pr-gate opened=false check all encode real operational lessons. That length is earned, not ceremonial.
