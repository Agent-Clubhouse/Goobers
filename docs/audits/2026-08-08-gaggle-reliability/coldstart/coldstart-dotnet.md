# Cold-start: dotnet

completed=True validate_clean=True tweaks=12 (validate 4, runtime 8), yaml_authored=185

## Summary
I got to a clean `goobers validate` (exit 0, one intentional warning) with a working fan-out review loop against Agent-Clubhouse/goobers-testbed-dotnet in about an hour, and the getting-started path — `init`, then `connect <owner>/<repo>` — is genuinely excellent: connect rewrote every placeholder across instance.yaml and the gaggle, recorded the token by env-var name only, and live-verified repo reachability, all in one non-interactive command. A shipped first-class .NET reference gaggle and a stack-support tier table meant I never had to invent the .NET story. The assigned pattern, though, is where the product falls down: `spec.parallels` exists and works, but it is documented only in a generated feature table, an implementer-facing design doc, and one example whose branches contain no gates — and it requires `dslVersion: "2.0"` when the binary itself reports 1.4 and every shipped example is 1.4, so a new user is unlikely to find it at all. Expressing "two reviewers, both must pass" is possible but inverted: a branch is a closed subgraph, so the canonical `needs-changes: implement` edge is illegal, and I had to add two stages whose only job is `exit 1`, two dead `next: "@join"` edges, and two byte-identical cloned park stages — about 55 of my ~90 fan-out lines are scaffolding, and the actual "both must pass" intent is nowhere written in the YAML. The sharpest safety finding is unrelated to fan-out: `requiredCapabilities: [nosuchtoolchain@42]` passes validate clean, so a typo'd or unclaimed toolchain token is a silent 100%-of-runs schedule-time failure the validator never mentions. Runner-up: `goobers workflow show` omits the parallel state entirely from its text projection (though `--dot` includes it), so the one command for eyeballing a workflow is blind to the hardest construct to author.

## Ledger

### 1. [docs-gap / via docs-contradiction]
1. Changed `dslVersion` from "1.4" to "2.0" so `spec.parallels` would be accepted.
- expected: The DSL version to declare is whatever the binary reports. `goobers schema` and `goobers schema workflow` both print `"dslVersion": "1.4"`, `goobers init` scaffolds `dslVersion: "1.4"`, and every shipped gaggle in config-examples/ (including the .NET reference) is 1.4.
- actual: `workflow.spec.parallels` only exists at DSL 2.0. Nothing in quickstart.md, arbitrary-repo-onboarding.md, stack-support.md, config-examples/README.md or `goobers --help` mentions that a DSL version above the reported one exists. I only found it by grepping docs/feature-matrix.md, whose 2.0 rows say `supported`, and by confirming with `goobers features --dsl-version 2.0`. `goobers examples list` offers backlog-assignment, backlog-curation, implementation, work-nomination — none uses parallels.

### 2. [dsl-ceremony / via validate]
2. Removed `needs-changes: implement` from both review gates — a reviewer inside a parallel branch cannot route a repass back to the implementation stage.
- expected: Copy the shipped acme-web `review` gate (`pass: local-ci / needs-changes: implement / fail: park-needs-human / escalate: park-escalated`) into each branch and get two independent reviewers with the same repass semantics.
- actual: `INVALID workflow: compile workflow "fanout-implementation": ... parallel "dual-review" branch "correctness" reaches its own onFailure state "implement"; the failure route must be outside every branch; parallel "dual-review" branch "correctness" reaches the join state "local-ci" directly; a branch must end at "@join" instead; parallel "dual-review" branch "correctness" routes back to the parallel itself; ...` — plus ~30 more clauses, all cascading from this one edge, emitted as a single unwrapped ~4500-char line with `;` separators and no dedup or root-cause ordering.

### 3. [dsl-ceremony / via guessing]
3. Invented two `reject-*` deterministic stages whose entire body is `exit 1`, purely to turn a `needs-changes` verdict into a branch failure the parallel's `failurePolicy: all_or_nothing` + `onFailure: implement` can route.
- expected: A gate verdict inside a branch could soft-fail its own branch, or there would be a reserved target like `@fail-branch` alongside `@join`/`@abort`/`@escalate`.
- actual: There is no such target. docs/design/static-fan-out-fan-in.md §5.2 says only "a branch that wants to fail softly returns a failed status and lets the parallel's failure policy decide" — the only way to produce a failed status from a verdict is to route the verdict at a stage that deliberately exits non-zero. No shipped example does this; the one shipped `parallels` user (reference-workflows/.../quality-sprint.yaml) is a research fan-out with no gates in its branches at all.

### 4. [dsl-ceremony / via validate]
4. Added `next: "@join"` to both `reject-*` stages even though that edge is unreachable by construction (the stage always exits 1).
- expected: A stage that always fails needs no successor.
- actual: Compile-time rule 2 (`state "X" cannot reach "@join", @abort, or @escalate (a branch may not complete the run)`) is pure graph reachability, so every branch state must declare a branch-terminal successor whether or not it can ever be taken. Dead YAML required to satisfy a static check.

### 5. [dsl-ceremony / via validate]
5. Split the single `park-needs-human` stage into two byte-identical copies, `park-needs-human-correctness` and `park-needs-human-tests`, one per branch.
- expected: Both reviewers' `fail` verdicts route to one shared 'park the issue as needs-human and abort' stage, exactly as acme-web's implementation.yaml does.
- actual: First attempt (routing both to `@abort` to escape the cross-branch error) produced `ERROR gaggles/dotnet-testbed/workflows/fanout-implementation.yaml Workflow/fanout-implementation: state "park-needs-human" is unreachable from start "query-backlog"`. Restoring the behavior required duplicating the stage because branch subgraphs must be disjoint. The design doc predicts this exact pain ("two branches whose gates both escalate cannot share one escalation state") but no user-facing doc does, and duplication scales linearly with branch count.

### 6. [default-assumption / via validate]
6. Stamped `workspace: repo-readonly` on both agentic gates and `workspace: scratch` under `run:` on every deterministic branch stage.
- expected: The default workspace works inside a branch, as it does everywhere else in the DSL.
- actual: `parallel "dual-review" branch "correctness": task "implement" resolves to a writable repo workspace; branch stages must use scratch or repo-readonly (concurrent repo-backed branches collide on the run branch)` — repeated once per task per branch (16 clauses in my first run). The built-in default (`repo`, writable) is illegal in any branch whenever `maxConcurrentBranches > 1`, and `maxConcurrentBranches` defaults to 1, i.e. the default for a construct named 'parallel' is sequential. Getting actual parallelism means opting into a knob that then invalidates the default workspace of every stage you put in it. Also note the syntax is asymmetric: `workspace:` is a task-level key for agentic tasks but a `run.workspace` key for deterministic ones.

### 7. [capability-expectation / via guessing]
7. Added `runner: {capabilities: [dotnet@8]}` to instance.yaml to match the gaggle's `requiredCapabilities: [dotnet@8]`.
- expected: If a gaggle declares a toolchain requirement and the instance claims nothing, `goobers validate` says so — docs/guides/arbitrary-repo-onboarding.md §6 lists "an unknown capability" among the typical validation failures.
- actual: Validate reports `OK: instance.yaml valid; config/ valid` with no runner block at all. I then set `requiredCapabilities: [nosuchtoolchain@42]` as a control and validate STILL printed `OK: ... 1 gaggle(s), 3 goober(s), 1 workflow(s)`. Per stack-support.md the scheduler then refuses to place every run at schedule time. So a typo'd or unclaimed capability token is a 100%-of-runs silent failure that the config validator is blind to. I only learned `runner.capabilities` exists from a commented-out block in reference-workflows/instance.yaml.example. Separately: which token to claim is undocumented — the host has SDKs 6/8/9 and the repo targets net8.0; whether `dotnet@8` matches an installed 9 SDK (or vice versa) is stated nowhere.

### 8. [default-assumption / via later-command-error]
8. Dropped the scaffold's `skills: [implement, run-tests]` block from every goober I wrote.
- expected: `goobers init` produces a warning-free starting point.
- actual: `goobers init` itself immediately printed `WARNING SKILL002 gaggles/example/goobers/coder/goober.yaml Goober/coder: spec.skills declares "implement", but no skill package directory was found at "gaggles/example/skills/implement" or "skills/implement"` (twice). The scaffold ships references to skill packages it does not scaffold. The shipped dotnet-service reference goobers have the same dangling `skills:` entries.

### 9. [invariant-assumption / via guessing]
9. Hand-edited three places to rename the gaggle from `example` to `dotnet-testbed`: the directory name, `metadata.name` in gaggle.yaml, and the `spec.gaggles` list in config/manifest.yaml — plus `spec.gaggle` in every goober and workflow.
- expected: `goobers scaffold` covers this; it has `goober` and `workflow` subcommands.
- actual: There is no `goobers scaffold gaggle` and no rename command. The directory name, `metadata.name`, the manifest entry, and every `spec.gaggle` back-reference must agree, and goober/workflow names are instance-global (documented only in arbitrary-repo-onboarding.md §10). `goobers connect` rewrites repo placeholders across gaggles but not identity.

### 10. [docs-gap / via cli-help]
10. Fell back to reading commented-out YAML in reference-workflows/instance.yaml.example to learn instance.yaml's shape.
- expected: `goobers schema instance` — the binary advertises `goobers schema` as a DSL entry point and instance.yaml is the first file a new user must edit.
- actual: `error: unknown schema kind "instance"` (exit 1). The kind list has gaggle, goober, manifest, workflow, verdict, result, journal-* and a dozen internal envelopes, but no `instance`. The only real documentation for instance.yaml is the comment block in reference-workflows/instance.yaml.example, which is genuinely excellent but is not discoverable from `--help` or the quickstart.

### 11. [docs-gap / via docs-contradiction]
11. Accepted a permanent `WARNING ... has no schedule trigger` (and therefore a permanently failing `validate --strict`) rather than adding a cron trigger.
- expected: Following the onboarding guide yields a clean validate.
- actual: docs/guides/arbitrary-repo-onboarding.md §4 says "Adjust cron schedules only after the manual acceptance run", and §7 walks you through `goobers run <workflow>`. Doing exactly that makes `goobers validate` warn on every invocation and makes `goobers validate --strict` report `configuration has 1 warning(s); --strict treats warnings as errors`. The recommended onboarding posture is the one the validator flags.

### 12. [docs-gap / via later-command-error]
12. Verified the fan-out topology with `goobers workflow show --dot` after the default text view turned out not to show it.
- expected: `goobers workflow show <name>` renders the DAG including the parallel — docs/design/static-fan-out-fan-in.md §5.1 states a parallel "is a state like any other: it is named, it is a valid next/branches target, and it appears in the graph projection."
- actual: The text projection lists every task and gate but omits `dual-review` entirely. It prints `implement (kind: agentic) -> dual-review` pointing at a state that is never listed, and shows nothing about the two branches, the join, `failurePolicy`, `onFailure`, `maxConcurrentBranches`, or `branchTimeoutSeconds`. `--dot` does emit the fan-out edges (`"dual-review" -> "review-correctness"`, `-> "review-tests"`, `-> "implement"`), so the two projections disagree. The one CLI command for eyeballing a workflow is blind to the exact construct that is hardest to author.

## Delights
- `goobers init` runs validation on itself and prints the warnings immediately, ending with an explicit `Next: edit these files before running a live workflow:` and the two file paths. Zero guessing about step two.
- `goobers connect Agent-Clubhouse/goobers-testbed-dotnet ./instance` did the whole placeholder rewrite across instance.yaml AND every gaggle, refused to accept a pasted token value by design, recorded the credential by env-var NAME only, and live-verified reachability: `REPOSITORY repos[0] Agent-Clubhouse/goobers-testbed-dotnet: reachable`. This is the single best thing in the first hour.
- A shipped, first-class .NET reference gaggle already exists (config-examples/gaggles/dotnet-service/) with `ciCommand: ["dotnet", "test"]` and `requiredCapabilities: [dotnet@9]`, and docs/guides/stack-support.md has an explicit stack tier table telling me .NET is 'Shipped, green'. I did not have to invent the .NET story.
- `goobers schema workflow` is genuinely self-documenting. The `parallel` def's description alone told me `failurePolicy` is required with no default, that `onFailure` is required for fail_fast/all_or_nothing and forbidden under continue_on_error, and that `maxConcurrentBranches > 1` requires scratch/repo-readonly workspaces — all before I ran validate once. The JSON Schema `if/then` encodes those rules machine-checkably.
- Validate distinguishes ERROR / WARNING / DSLVERSION / REPOSITORY / HARNESS / PLACEHOLDER001 / SKILL002 with stable codes and file paths, and `PLACEHOLDER001 instance.yaml: contains unedited template marker(s) your-org, your-repo` is a lovely proactive nudge.
- `goobers validate --check-harness` printed `HARNESS copilot: OK` — a real preflight of the agent harness sign-in, not just a YAML lint. Same for `--check-repos`.
- The heavily-commented reference-workflows/instance.yaml.example and config-examples/gaggles/dotnet-service/*.yaml explain WHY, not just what — including the exact sentence that told me the local-ci task's `command: ["make","ci"]` is deliberately overridden by the gaggle's `ciCommand`. Without those comments I would have mis-set the CI command.
- The parallel-branch error messages, once you get past the volume, are unusually specific and prescriptive: `a branch must end at "@join" instead`, `the failure route must be outside every branch`, `branch subgraphs must be disjoint so every event belongs to exactly one branch (concurrent repo-backed branches collide on the run branch)`. Each one names the rule and the reason.

## Docs notes
The docs are unusually good in aggregate and specifically bad on the one thing I was asked to do.

What worked: quickstart.md -> arbitrary-repo-onboarding.md is a real, correct, production-oriented path, and stack-support.md answered "is .NET supported" in one table row. The commented instance.yaml.example and the config-examples gaggle YAML are the best documentation in the product.

The gap: `spec.parallels` — the fan-out/fan-in primitive — is not documented for users anywhere. It appears in exactly three places: (1) docs/feature-matrix.md, a generated table that only tells you the field exists at DSL 2.0; (2) docs/design/static-fan-out-fan-in.md, a design doc written for implementers (it cites Go file paths and line numbers, discusses `IsReservedTarget` predicate refactors, and is where I had to go to learn the `@join` vocabulary, the failure-policy table, and the 8 compile-time rules); (3) reference-workflows/gaggles/goobers/workflows/quality-sprint.yaml, one example whose branches contain no gates at all, so it does not demonstrate a review fan-out. There is no guide page, no `goobers examples` entry (the 4 embedded examples are all linear), and no mention in quickstart.md, arbitrary-repo-onboarding.md, custom-stage-cookbook.md, or dsl-authoring-skill.md. A new user who does not think to grep docs/design/ simply will not discover that parallel branches exist, and one who does is reading an implementation spec.

The DSL-version discoverability problem compounds this. `goobers schema` and `goobers schema workflow` both report `"dslVersion": "1.4"`, `goobers init` scaffolds 1.4, and all 4 shipped gaggle references are 1.4 — so the honest inference is "1.4 is the DSL", and 1.4 has no parallels. Only feature-matrix.md's 2.0 rows (marked `supported`) reveal otherwise. Nothing tells a user that declaring a higher `dslVersion` is allowed, let alone required, for a whole class of features (`parallels`, `stage.workspace`, `stage.run.script`, `gate.evaluator.agentic.workspace`, `task.inputsFrom.stageQualified`).

Three smaller gaps. (1) instance.yaml has no published schema (`goobers schema instance` -> `error: unknown schema kind "instance"`) despite being the first file you must edit. (2) The agentic-stage output convention is undocumented: quality-sprint pairs `inputs.artifactFile: findings.md` with `expectedOutputs: [findingsRef]` and consumes it as `focus-areas.security.review-security.findingsRef`, but nothing explains where the name `findingsRef` comes from or whether an agentic stage can emit arbitrary scalar outputs — which is why I avoided a verdict-passing design. (3) Repass bounding: `DefaultMaxRepasses = 3` appears only in docs/design/human-in-the-loop.md, and whether a parallel's `onFailure` route counts against a repass budget is stated nowhere, so I cannot tell whether my implement -> dual-review -> implement loop is bounded.

Finally, the validate output for a bad parallel is one unwrapped ~4500-character line, `;`-separated, with every consequence of a single wrong edge listed separately and per-branch (my first run emitted roughly 35 clauses, ~16 of them the same workspace complaint repeated per task per branch). The individual messages are excellent; the presentation buries the one edge that caused them all.

## DSL ceremony notes
The DSL CAN express a two-perspective parallel review gate, but only by inverting how you'd naturally write it, and 5 of my 12 tweaks are pure ceremony from that inversion.

A parallel branch is a closed, disjoint subgraph whose only exits are `@join`, `@abort`, `@escalate`. Gates are perfectly legal inside branches, but a gate's verdict targets must also be branch-terminal — so the single most important edge in any review loop, `needs-changes: implement`, is structurally illegal inside a branch. The review verdict cannot leave the branch as a verdict; it can only leave as a branch *status*. That forces this shape:

  gate review-correctness  pass -> "@join"
                           needs-changes -> reject-correctness   (a stage that exists only to `exit 1`)
                           fail -> park-needs-human-correctness  (a per-branch clone of a shared stage)
                           escalate -> "@escalate"
  task reject-correctness  run.script: `exit 1`, workspace: scratch, next: "@join"  (dead edge, required by the reachability check)

...times two branches, then `failurePolicy: all_or_nothing` + `onFailure: implement` at the parallel to carry the repass. The semantics come out exactly right — both reviewers must pass, either rejection repasses implementation — but the intent ("both must pass") is nowhere written down; it is an emergent property of a failure policy plus two stages whose job is to fail. A reader of this YAML cannot see the pattern.

Concretely, the ceremony tax for 2 review perspectives is: 2 exit-1 stages (22 lines), 2 duplicated park stages (28 lines), 2 dead `next: "@join"` edges, and 4 `workspace:` declarations that exist only because the default is illegal here. That is ~55 of my ~90 fan-out-specific YAML lines — more than half the construct is scaffolding, and it scales linearly with branch count.

What would have removed almost all of it: (a) a reserved `@fail-branch` target so a gate verdict can soft-fail its own branch directly; (b) permitting reserved-terminal-like *shared* park stages, or auto-cloning them per branch; (c) making the join able to see per-branch gate verdicts (it gets `inputs.branchCompleteness`, but that carries terminal status and artifact counts, not verdicts, and there is no documented way for a deterministic join script to read a structured input at all — inputs reach scripts as `GOOBERS_INPUT_*` scalars). (c) is why I could not use the more natural `continue_on_error` + deciding-join design.

Two smaller ones. `maxConcurrentBranches` defaults to 1 — a construct called `parallel` is sequential unless you opt in, and opting in is what makes the default `repo` workspace illegal, so "make it actually parallel" and "rewrite every stage's workspace" are the same decision. And `workspace` is a top-level task key for agentic tasks but `run.workspace` for deterministic ones, which I got wrong once from muscle memory.
