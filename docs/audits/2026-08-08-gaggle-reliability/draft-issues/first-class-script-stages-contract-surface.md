# Script stages get a bare shell, not a stage contract — no run identity, no repo identity, no scoped credentials, no declared failure vocabulary

Suggested labels: area:workflows, area:runner, area:contracts, type:feature, sprint:v1-breadth

## Problem

A workflow author who writes `run.script` gets a shell with the daemon's
ambient environment and nothing else. Every contract a built-in stage relies
on stops at the script boundary:

- **Repo identity.** A script has no idea which repository its run targets.
  A provider CLI invoked from it resolves the repository from the process
  working directory. When the working directory happens to sit inside some
  other git working tree, the script does real work against the wrong
  repository, finds nothing, and the stage exits zero. The run is green, the
  workflow reports no-work, and no status anywhere in the system is wrong.
- **Run identity.** A script cannot say what produced it. Provenance in an
  artifact or an issue footer is whatever the author hardcoded or the agent
  guessed.
- **Credentials.** Declared capabilities materialize into `GOOBERS_CRED_*`
  variables that no third-party CLI reads, while the ambient allowlist
  carries `HOME` — so a provider CLI in a script stage authenticates as
  whatever identity the *host* is logged in as, outside the per-gaggle
  credential scoping the runtime otherwise enforces correctly. The declared
  capability set of a script stage is decorative.
- **Inputs and outputs.** A script can reference an environment variable the
  runtime never injects for its stage kind; under `set -u` that is an
  immediate exit 1, reported as a missing result file. Nothing at author time
  compares what a script reads against what its stage kind receives.
- **Failure classes.** A typed-failure channel exists but is reachable only
  by declaring a result file and knowing three undocumented key names; the
  channel built-ins use is explicitly an internal subprocess protocol. Script
  failures therefore land in the generic `nonzero_exit` /
  `missing_result_file` buckets, which retry routing cannot act on.

The consequence is asymmetric trust: the runtime is strict about built-in
stages and unconditionally credulous about script stages, in a product whose
ratified direction is minimal curated built-ins **plus** first-class script
support. Today "first-class" means "the field parses."

## Evidence

- `domains/audit-nomination-flows.md`, `test-instability-wrong-repo`
  (CRITICAL/confirmed): 55 runs of a flagship nomination workflow analyzed
  the CI history of the config repo instead of the target repo, because a
  bare provider CLI ran in a scratch workspace nested inside the config
  repo's working tree. Every run: green, `completed`, `no-work`. Mechanism:
  `cmd/goobers/runnerwiring.go:2259` (`ScratchDir` under `WorkcopiesDir()`),
  `internal/runner/run.go:3921` (`MkdirTemp` there); `grep -rn GH_REPO
  internal/ cmd/` returns zero hits, so the CLI has only the working
  directory to resolve from.
- `domains/audit-nomination-flows.md`,
  `upstream-sync-failed-run-rootcause` (MEDIUM/confirmed): a deterministic
  script stage read `$GOOBERS_ADDITIONAL_REPO_GOOBERS` under `set -u` and
  died in 0.26s (`unbound variable` → `missing_result_file`). The env family
  is named at `internal/executor/env.go:96-100`; injection was gated on
  `injectRunContext`, set only when `command[0] == "goobers"`. The env half
  is filed and closed as #2606; the workspace half remains open as #2605,
  with #2607 covering the validate blindness for the same grants.
- Run-identity boundary: `internal/executor/env.go:135-143` documents the
  deliberate least-privilege rule — run context reaches goobers-CLI stages
  only, so a project's own test suite is not perturbed. `domains/
  audit-code-audit.md`, `run-identity-wish6`: the agentic half shipped for
  one harness only (`internal/mcpio/tools.go:37-53`,
  `internal/harness/copilot_mcp_io.go:12-14`, vs `internal/harness/claude.go:124-203`),
  filed and closed as #2617. Script stages were never in scope of either.
- Ambient credentials: `internal/procenv/procenv.go` `Vars` carries
  `PATH`/`HOME`, so a provider CLI reads the host user's stored login;
  declared capabilities materialize as `GOOBERS_CRED_*`
  (`internal/executor/env.go:17-26`, `224`), which that CLI does not read.
  The agentic path does map capabilities onto the variable the CLI actually
  reads (`cmd/goobers/runnerwiring.go:318`, `359-370`); the deterministic
  path has no equivalent. Blast radius of the resulting single identity:
  `domains/audit-nomination-flows.md`, `shared-user-rate-limit-blast-radius`
  (one user's REST bucket shared by both gaggles, the operator's CLI, and
  the harness).
- Typed failures: `internal/executor/shell.go:92-120` (`noWork`,
  `errorCode`, `errorMessage`, `errorRetryable` — reachable only with a
  declared `resultFile`), `internal/executor/env.go:64-67`
  (`GOOBERS_BUILTIN_ERROR_FILE` — "an internal subprocess protocol, not a
  DSL input"). Downstream effect:
  `domains/audit-nomination-flows.md`, `harness-transient-no-retry-inconsistency`
  (identical executor errors retried or not depending on flavor) and the
  audit README's observability theme (agents inventing seven free-form codes
  for one cause).
- `coldstart/coldstart-minimal.md`, "Docs notes" and "DSL ceremony notes":
  `ci-poll` requires a schema-mandated `run.command` placeholder that never
  executes (`goobers ci-poll` is not a command), and its whole authoring
  contract exists only as comments inside one example file. The same
  ceremony tax lands on script authors: 234 lines of YAML for the smallest
  end-to-end loop, six of whose items are plumbing the runtime already knows.
- DSL placement: `run.script` is 2.0-only —
  `internal/workflow/v_current/compile.go:223` rejects it,
  `internal/workflow/v_next/features.go:439` registers
  `stage.run.script`. The version is being consolidated under #2695, which
  is where this surface belongs.

## Proposed direction

Give `run.script` the contract surface built-ins already have. This is stage
metadata on the existing task shape in DSL 2.0 — not a new stage kind, not a
plugin seam.

1. **Repo identity, injected and pinned.** Any stage that declares a
   provider capability (or opts into identity) receives the run's routed
   repository as both provider-neutral coordinates and the provider CLI's
   own native pin variable, so a bare CLI call cannot resolve a repository
   from the working directory. Pair this with moving the scratch workspace
   root outside any git working tree: with no ambient repo to infer, a
   missing pin fails loudly instead of succeeding against the wrong target.
2. **Run identity by default for scripts.** A `run.script` stage is written
   by the operator, for the runtime — it is not the project's own test
   suite, which is what the current least-privilege boundary protects.
   Scripts get run/workflow/gaggle/stage identity by default with an
   explicit opt-out; `run.command` keeps today's boundary with an explicit
   opt-in. Provenance stops being a thing an agent or a hardcoded string
   guesses.
3. **Scoped credentials instead of ambient login.** A declared credentialed
   capability materializes a token into the variable the provider's CLI
   actually reads, resolved through the same per-gaggle grant path the
   runtime already gets right per run — and the stage runs with the host's
   credential-store locations redirected to a run-scoped directory, so an
   *undeclared* capability cannot silently fall through to the operator's
   ambient login. Declaring nothing means having no provider access, which
   is the failure worth having on the first run.
4. **Declared inputs and outputs, validated.** A script stage declares the
   result file, the outputs it emits, and the run-context families it
   consumes. Validate rejects a downstream `inputsFrom` referencing an
   output no producer declares, and rejects a declared consumption of a
   context family the stage's kind and workspace cannot receive — the class
   that currently exits 1 on the first line under `set -u`.
5. **A published failure vocabulary.** The typed-failure keys become
   documented DSL surface with a known code enum; a code outside the enum
   journals as explicitly unclassified rather than being silently accepted,
   and retry routing keys on the class rather than a single retryable
   boolean.

**Zero-config behavior.** Writing six lines of shell and nothing else gets:
run identity env, repo coordinates plus the provider CLI pin when the gaggle
has an unambiguous repo, a default result-file path read if present, no
provider credentials, no ambient host login, and today's generic exit-code
failure semantics. Every richer property — a second capability, a typed
failure code, extra declared outputs — is one field. The author who wants
none of it types none of it; the author who wants all of it does not leave
the YAML.

## Alternatives considered

- **Documentation and a cookbook entry** telling authors to pass a repo flag
  and export a token. Rejected: the failure mode is a green run. Prose
  cannot make a validator catch it, and the audit shows the documented-but-
  unchecked pattern is exactly what produced 55 false-green runs.
- **Only move the scratch workspace out of any git working tree.** This is
  the cheap half and should ship inside this work, but on its own it fixes
  repo inference and leaves identity, credentials, and failure classes.
- **Route all custom logic through a registered stage-kind plugin seam.**
  Rejected: contradicts first-class script support, and a plugin registry is
  a much larger surface that does nothing for an operator who wants six
  lines of shell.
- **Allowlist the provider CLI's token variable ambiently.** Rejected:
  reinstates one shared identity across every gaggle and reintroduces the
  cross-gaggle quota coupling the per-run grant model already avoids.
- **Wait for containerized stage execution to supply isolation.** Rejected
  as a prerequisite: containers change where a script runs, not what it is
  told; every item above is still needed inside the container.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers` (open and closed),
terms: `run.script`, `script stage`, `script stage contract`, `GH_REPO`,
`scratch workspace`, `run identity`, `get_run_info`, `typed failure class`,
`custom stage`, `expectedOutputs`, `wrong repository`, `deterministic stage
credentials`, `repo coordinates env`, `stage credentials scope`.

Nearest existing issues and the delta:

- **#744 (open, EPIC — Custom & Generic Stages)** is the parent catalog and
  the right home. Its children cover runtime provisioning (#735, closed),
  the env allowlist (#736, shipped as `runner.envPassthrough`), a stage-kind
  registration seam (#737), the dead image field (#734), and cross-run
  caches (#738). None of them gives a script stage identity, repo pinning,
  scoped credentials, or a failure vocabulary.
- **#1539 (closed)** added the inline `run.script` field and cookbook
  coverage — the syntax, not the contract. This proposal is what the field
  still lacks.
- **#2606 (closed)** injected `GOOBERS_ADDITIONAL_REPO_*` into deterministic
  stages; **#2605 (open)** covers reference checkouts for scratch
  workspaces; **#2607 (open)** covers validate blindness for those grants.
  The reference-repo family is therefore already filed — **this draft is
  narrowed to exclude it** and cites it as adjacent.
- **#2617 (closed)** delivered `get_run_info` for agentic stages under one
  harness. Deterministic and script stages were out of scope, and remain
  identity-blind.
- **#565 / #907 (closed)** established that `expectedOutputs` is enforced by
  nothing and warned on the inert field. This proposal asks for enforcement
  at the one boundary where an author writes the producer by hand.
- **#2645 (open)** reports the custom-stage cookbook's sample is Unix-only —
  a docs defect in the same area, not a contract change.
- **#2695 (open, EPIC — DSL 2.0)** is where this surface lands: `run.script`
  is already 2.0-only, so the contract fields can be added without touching
  the frozen 1.4 interpreter.

Nothing found covers the script-stage contract surface itself.

## Size and risk

**L** overall; naturally splits into four independently shippable pieces
(repo identity + scratch relocation; run identity; scoped credentials;
declared outputs and failure vocabulary). The first is small and retires the
CRITICAL false-green class on its own.

Blast radius: stage environment construction, credential injection, scratch
workspace placement, the workflow schema, and validate. All are per-stage
paths; no scheduler or provider changes.

Migration notes: every new field is optional and defaults to today's
behavior, except two deliberate changes. (1) Script stages begin receiving
run-identity variables — additive, and the least-privilege rationale
documented at `internal/executor/env.go:135-143` applies to a project's own
build command, which keeps its current boundary. (2) Redirecting the host
credential store away from stage subprocesses breaks any instance that
today relies on the daemon host's ambient CLI login; ship it as a validate
warning naming the stages that would lose access, then flip it with DSL 2.0
adoption so the change is versioned rather than silent.
