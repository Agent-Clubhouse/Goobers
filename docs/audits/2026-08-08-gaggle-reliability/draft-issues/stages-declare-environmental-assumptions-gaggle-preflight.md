# Stages don't declare their environmental assumptions, so a config whose every run will time out, mis-resolve its toolchain, or be refused can still validate clean

Suggested labels: area:runner, area:contracts, area:onboarding, type:feature

## Problem

Every stage makes assumptions about the world it runs in: which binaries
must be on its PATH at which version, how long it is allowed to take, which
provider API permissions its calls require, which workspace it needs. Those
assumptions live in the engine's code and in prose guides. The author's
config can contradict all of them and still validate clean, because nothing
compares a stage's real requirements against a specific gaggle's host,
token, and repository.

The result is a family of failures that are 100%-of-runs deterministic and
individually unrecognizable:

- A stage inherits a default deadline sized for a short command and runs a
  full test suite. Every run times out. The surface symptom is an escalation
  storm, not a configuration error.
- A stage's command resolves an interpreter from a login shell's PATH, gets
  a different version than the one the config declares and the gaggle
  requires, and fails later with a packaging error that names neither.
- A stage requires a toolchain token no runner claims. The config validates
  OK; the refusal arrives later, from a different component.
- A stage's provider calls need an API permission the gaggle's token cannot
  hold. Every call 403s from the moment the workflow first reaches that
  stage.
- A stage needs environment variables for a stack the ambient allowlist does
  not cover. Nothing states which stacks are covered, so the author guesses.

The common shape: the tooling already contacts the repo, already knows the
stage's requirements, and already records how long stages actually take —
and still makes the author find out by running it.

## Evidence

- `domains/audit-goobers-runs.md`, `pr-remediation-localci-timeout`
  (CRITICAL/confirmed): the substantive remediation lane went 0-for-106 in
  August — 92 timeouts (all at exactly 10 minutes) and 14 nonzero exits —
  because one workflow's local-CI stage kept the engine default while its
  sibling was raised, and the suite now takes 13m18s. Default:
  `internal/boundedwait/boundedwait.go:15` (`DefaultTimeout = 10 *
  time.Minute`). 79 of the 144 escalations in the window trace to it. One
  missing line, no signal anywhere at author time.
- `coldstart/coldstart-python.md` #5 and #6: with `requiredCapabilities:
  [python@3.12]` declared and `runner.capabilities: [python@3.12]` claimed,
  the stage's actual `sh -lc` login-shell PATH resolved `python3` to 3.9.6
  against a package declaring `requires-python >=3.12`; `validate --strict
  --check-harness --check-repos` was clean throughout, and there is no way
  to ask the tool what a stage's PATH will resolve to. Same ledger: the
  reference stack's `ciCommand` cannot express install-then-test, because it
  is a single argv.
- `coldstart/coldstart-swift.md` #7 and #8: the ambient allowlist
  (`internal/procenv/procenv.go` `Vars`) covers Go/.NET/Python/Node/Rust/
  Java and not Swift/Xcode, and no shipped doc says which stacks are
  covered; the stage timeout was raised preemptively to 25m for a repo that
  builds in 3.5s, because nothing could report either the requirement or the
  reality before a run.
- `coldstart/coldstart-dotnet.md` #7: `requiredCapabilities:
  [nosuchtoolchain@42]` with no `runner:` block at all validates OK. The
  fail-closed checks exist elsewhere — daemon startup
  (`cmd/goobers/daemon.go:184`) and the schedule tick
  (`internal/localscheduler/scheduler.go:1651`) — just never at author time.
  Which token to claim for an installed SDK is documented nowhere.
- `coldstart/README.md`, findings 1 and 2: only 32% of 44 cold-start
  frictions were caught up front; the recurring shape is "the trap is
  documented and unchecked." Probe results in the same section: a full
  ci-poll workflow against a repository with no CI validates clean, and a
  gate's `fail:` branch is silently unreachable without `continueOnError`.
- Provider-API capability class: `README.md` regime 3 — a fine-grained token
  that cannot call the Checks API produced 490 stage and 496 scheduler
  failures in one 13-hour window, starting the minute the gaggle opened its
  first PR. The fallback is merged (#2685 / PR #2689); nothing preflights a
  token's API surface per gaggle, so the next capability gap presents
  identically.
- Precedent already in the tree: provider-capability requirements *are*
  checked at author time (`cmd/goobers/validate.go:335`,
  `instance.CheckProviderCapabilityRequirements`, CONF-6/#2079) with exactly
  this rationale — an unmet requirement can never self-heal at runtime, so
  it should fail at validate rather than mid-run. Toolchain, duration,
  environment, and workspace assumptions have no equivalent.
- Scope note on overlap: a static cross-check for the unclaimed-capability
  case above (warning CAP003) plus repo-reality checks for selectors and
  ci-poll are implemented on the reliability-hardening branch
  (`cmd/goobers/validatereality.go`, `api/validate/validate.go`). This
  proposal is the general framework those individual checks should become
  instances of, not a re-filing of them.

## Proposed direction

Make environmental assumptions a declared property of a stage — the way
capabilities are declared today — and verify them against each gaggle's own
host, token, and repository at config-apply time. Three classes, three
treatments:

**1. Defaults — observe and derive.** Deadlines, poll intervals, and
concurrency ceilings are guesses until the instance has run. The runtime
already records per-stage durations for every attempt. `validate` and the
doctor surface compare each stage's effective ceiling against the observed
distribution for that same stage in this instance and warn when the ceiling
sits below the observed high percentile, naming the run that proves it.
Replace the single engine-wide numeric default with duration *classes*
(quick / build / long / polling) that stages declare and the instance maps
to seconds once; an explicit per-stage value still wins. A stage that polls
a provider and a stage that runs a full suite stop sharing one ten-minute
ceiling, which is the specific defect that killed the remediation lane.

**2. Invariants — pin at construction.** Anything a stage's correctness
depends on that must not be re-inferred per run: the interpreter or binary a
command resolves to, the repository it addresses, the shell it runs under.
These are resolved once when config is applied, recorded as absolute,
version-stamped values in the effective config, and re-checked on reload —
so "which python3" is answered in one place at one time, not by whatever
PATH a login shell happens to build inside a worktree.

**3. Capability expectations — probe per gaggle.** Toolchain family and
version tokens (the existing requiredCapabilities vocabulary), the
environment-variable families a declared stack needs, the workspace shape a
stage requires, and the provider API permissions its calls will make. At
config-apply — and on the existing network-checking validate flag — each
gaggle's (host, token, repository) tuple is probed independently: does the
toolchain resolve at the declared version under the stage's real
environment; does the allowlist cover the declared stack; does this token
hold the permissions these stages will exercise. Results are per gaggle. An
instance with one good token and one bad one must say so.

**Built-in stages ship their own declarations.** Local-CI declares that its
toolchain comes from the gaggle's CI command, that it is a long-duration
stage, and that it needs the project workspace. CI polling declares the
provider read permissions it uses and that it is a polling-duration stage.
Backlog stages declare their issue permissions. The declaration belongs to
the stage, not to the config that uses it.

**Zero-config behavior.** An author who declares nothing still gets every
built-in stage's own assumptions checked against their gaggle: the toolchain
probe, the token permission probe, the stack-coverage check, and a duration
class appropriate to each stage instead of one global default. Declarations
are needed only for script stages and for deliberate overrides — the
progressive-disclosure knob, not the entry price.

**Disposition.** An unmet expectation that is statically knowable (a token
that cannot hold a required permission, a capability no runner claims) is a
validate error. A host observation (a toolchain resolving to the wrong
version, a ceiling below observed durations) is a validate warning that
names the evidence. At run time, an unmet environmental expectation is a
stage failure — never a per-item block that parks a whole claimed batch.

## Alternatives considered

- **Provision the toolchain instead of checking it** (version managers,
  installers, or containers per stage). Rejected as the first move: it is
  host-invasive and heavy, and it addresses only one of the four classes —
  token permissions, duration classes, and workspace expectations are
  untouched. Checking is the cheap majority of the value, and a container
  route can later implement the invariant pin rather than replace it.
- **Document the requirements per stack.** Rejected: the cold-start
  exercise found the prose already documents most of these traps and 68% of
  friction was still discovered by guessing or by a later command's error.
- **Extend the capability-token matching language** (richer predicates over
  requiredCapabilities). Rejected as sufficient: capability tokens describe
  toolchains only, and toolchains are one of four assumption classes in the
  observed failures.
- **Preflight at run time only** (fail fast at stage start). Better than
  today, and worth keeping as the backstop, but the operator still learns
  after dispatch — while the same facts are available when config is
  applied.
- **Derive everything, declare nothing** (infer requirements from the
  command line). Rejected: inference is exactly what produces the wrong
  interpreter today; declarations are what make the check meaningful and
  the failure message actionable.

## Duplicate search

Searched 2026-08-08 against `Agent-Clubhouse/Goobers` (open and closed),
terms: `preflight`, `toolchain`, `requiredCapabilities`, `capability
sufficiency`, `unclaimed capability`, `stage timeout`, `envPassthrough`,
`validate PATH`, `validate environment`, `ciCommand`, `doctor`,
`fine-grained PAT`, `token permission check`, `workspace scratch`, `declare
assumptions`.

Nearest existing issues and the delta:

- **#2079 (closed, CONF-6)** built workflow provider-capability
  requirements plus scheduler preflight refusal. That is the enforcement
  half for one class, and the precedent this generalizes: the same
  author-time check does not exist for toolchains, durations, environment
  families, or token permissions.
- **#2197 (open)** proposes a deterministic capability preflight for
  agentic stages and reclassifies missing-capability conditions away from
  the per-item block disposition. Genuine overlap on disposition, narrower
  in scope (harness tool surface, run time). Recommendation: keep #2197 as
  the agentic instance and land its disposition rule there; this proposal
  covers the environmental classes and moves detection to config-apply.
- **#735 (closed, completed)** asked for declarative stage runtime
  *provisioning* and closed via the declare-and-refuse route
  (requiredCapabilities plus the toolchain verifier). This is the missing
  third step: verify the declaration against the gaggle before it runs.
- **#1529 (open)** explores CEL-based predicates for capability matching —
  a matching-language change inside one class; orthogonal and compatible.
- **#1177 / #917 (closed)** added static timeout-coherence checks against
  bounded waits. Structural only: they compare declared numbers to each
  other, never to observed stage durations, which is what the 0-for-106
  case needed.
- **#1070 (closed)** removed the hardcoded harness session timeout in favor
  of configurable defaults — the harness twin of the duration-class idea,
  for one stage type.
- **#2064 (open)** is the tech-stack neutrality tracker (Java/Python
  first-class, honest tier docs). Adjacent and complementary: it makes the
  stacks work; this makes the config say whether a given host can run them.
- **#2542 / #2071 / #2173 (closed)** made init and guided setup emit
  requiredCapabilities and a real CI command — the authoring half of the
  same problem, with no verification step.
- **#2172 (closed)** fixed stage-timeout diagnostics being Go-keyword-only;
  diagnostics after the fact, not prevention.
- **#1102 / #662 / #1494 / #734** are the container/image route — the
  alternative rejected above as a first move.
- **#2645 / #2634 (open)** are example/doc defects in the same area.

Nothing found proposes stage-declared environmental assumptions verified
per gaggle at config-apply.

## Size and risk

**M–L**, and cleanly splittable: the duration-class change and the
observed-duration warning are each **S–M** and independently valuable; the
per-gaggle probe framework is **M**; retrofitting declarations onto every
built-in stage is **M** and incremental, stage by stage.

Blast radius: the workflow and instance schemas (new optional declarations),
validate, config apply/reload, and the executor's timeout resolution. The
scheduler's existing refusal paths are untouched — this adds earlier
detection, not new enforcement points.

Migration notes: all declarations are optional; duration classes map to
today's numeric defaults so an unchanged config behaves identically. New
warnings count under strict validation, so land them as warnings with a
named suppression path and a release note before any promotion to error.
Probing at config-apply introduces network and subprocess work on reload —
run it out of band, cache per gaggle, and never block a reload on a probe
that times out, matching the existing rule that repo-state observations
never change the exit code.
