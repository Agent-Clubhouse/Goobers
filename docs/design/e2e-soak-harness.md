# Design: an e2e soak harness for load-dependent failures

Status: draft — proposed design decision. Filed against #815; splits into #1479 (driver),
#1480 (isolated environment), #1481 (scheduled workflow). No implementation lands
until this document does.

## 1. Motivation, precisely

The `implementation` workflow's `local-ci` gate hung intermittently at its 10-minute
stage timeout, always at the identical spot in `go test -race ./...` output, and
only when **2+ `local-ci` stages ran concurrently** on the self-host machine (#811).
A solo run always passed in about a minute. The first fix (#814) diagnosed a
disk-I/O theory — `t.TempDir()` teardown's `os.RemoveAll` blocking in
uninterruptible kernel I/O under concurrent disk saturation — and shipped a
`serializeGroup` executor mechanism to serialize the contended stage.

That diagnosis was wrong. The #845/#846 post-mortem reproduced the identical hang
at a **single serial run** — ruling out disk contention entirely — and traced the
real cause to terminal job control: the daemon spawned the test process with
`Setpgid`, and a background process group sharing the daemon's controlling
terminal gets `SIGTTOU`-stopped by the kernel the moment it touches that terminal,
independent of load. The real fix was `Setsid` (#850). `serializeGroup` was fully
reverted (#853/#857) once it was shown to treat a symptom, not the cause.

This design does not exist to catch "the #811 bug" specifically — that bug is
fixed, and re-litigating it teaches nothing. It exists because **the diagnosis
took two attempts, and the first attempt's fix looked plausible, was reviewed,
shipped, and was still wrong** — because nothing in CI ever contradicted it. A
solo `make ci` run is a poor witness for a bug that only exists under contention.
The harness's job is to be a better witness: a repeatable way to run real work
under real concurrent load and honestly report whether anything wedges, races, or
degrades — so the next load-dependent bug gets a real reproduction attached to its
issue on day one, not a plausible theory that ships wrong.

## 2. What exists today (verified against `origin/main`)

Three related harnesses already exist. None of them do what this design needs, but
each sets a precedent this design follows rather than reinvents:

- **`test/stress/`** (`.github/workflows/stress.yml`): repeats a small, curated
  set of timing/goroutine-sensitive packages (currently just
  `internal/localscheduler`, annotated "site of the timing incident tracked by
  #21") under `go test -race -count=20` by default. A reviewed per-package
  `count=N` override may lower repetitions only when its full race suite cannot
  fit the 90-minute job budget. It runs nightly plus `workflow_dispatch`, uploads
  a `stress-results/` artifact `if: always()`, and feeds a
  `flake-ledger` job — gated on `github.event_name == 'schedule'` — that files or
  updates a GitHub issue through `test/flakeledger`. This is repetition under the
  race detector, not concurrent resource load; it has never caught a
  contention-only bug like #811 because it runs one package's tests repeatedly in
  one process, never N concurrent full-suite processes fighting over a host.
- **`test/scale/`** (`.github/workflows/large-repo-scale.yml`): a committed
  load/scale harness for the portal read path (#1912/#1913). Its `LoadSpec`
  (`test/scale/mixedload.go`) parameterizes concurrent write load — scheduler
  journal appends, run journal appends, rollup ingest — applied while reads are
  measured, so a read's cost under contention can be compared to its cost alone.
  Its own doc comment states the principle this design adopts wholesale: it
  "deliberately uses the REAL `journal.InstanceLog.Append` rather than the
  generator's direct write... using the fast path here would remove the defect
  from the experiment designed to detect it." `test/scale/adverse.go` reports an
  injected-delay result explicitly tagged `SIMULATED`, distinct from a measured
  one, with its own doc comment: "The number bounds the decision; it does not
  settle it." Weekly cadence, 90-minute budget, artifact uploaded `if: always()`.
- **`internal/sandbox`** (exercised by the `sandbox confinement` CI job, `make
  sandbox-check`): a platform-neutral filesystem-write jail for one agentic
  harness subprocess — bubblewrap user namespaces on Linux, Seatbelt on macOS.
  It confines what a stage's Claude/Copilot subprocess can write to, and carries
  **no CPU, memory, disk-I/O, or network limiting of any kind** (`Policy` is
  `{Workspace, WritableRoots}` only). It is not resource isolation and this
  design does not treat it as such.

No existing code in this repository deliberately saturates CPU, memory, or disk
I/O. No cgroup, rlimit, container, or VM primitive exists anywhere in the tree.
§3 has to introduce one.

## 3. Isolation strategy

**Decision: a single, exclusively-reserved Docker container per soak run, on
Linux, with `--cpus`, `--memory`, and blkio (`--device-read-bps` /
`--device-write-bps`) limits set from the load profile (§6).**

Why Docker over the alternatives actually available:

- **A dedicated bare host with no container** was `test/stress`'s and
  `test/scale`'s choice, and it is right for them — neither needs to *inject*
  resource pressure, only to run real work and observe it. This harness's whole
  point is deliberately constraining CPU/memory/disk-I/O to a known, reproducible
  ceiling, which a bare host cannot do without external tooling per platform
  (`cgroups` on Linux, nothing comparable and portable on macOS/Windows).
- **Raw Linux cgroups v2** (no container) would work and is what Docker uses
  underneath, but authoring and tearing down cgroup hierarchies directly is
  Linux-only infrastructure this design would have to build and maintain from
  scratch, for a smaller feature surface than what a container runtime already
  gives for free (namespaced filesystem, deterministic base image, the same
  resource flags, plus a cleanup guarantee on container removal).
- **A micro-VM (Firecracker et al.)** gives the strongest isolation but needs a
  KVM-capable host, is unavailable on GitHub-hosted `macos-latest`/`windows-latest`
  runners, and is new infrastructure this team has never operated. Revisit only
  if Docker's isolation proves too leaky in practice (§14 names this as a
  reconsideration trigger, not a current problem).

**Portability limits, stated plainly:** this is a **Linux-first, v1** decision.
GitHub-hosted `ubuntu-latest` runners have Docker preinstalled and support the
resource-limit flags this design uses; that is the only execution target for
v1. macOS and Windows do not get real resource-pressure injection in this
version — Docker Desktop's own VM layer on those platforms changes the resource
model enough (a shared VM, not native cgroups) that a "CPU-limited container"
there is not comparable evidence to the Linux one, and #811-class bugs
(Linux-daemon process-group/terminal semantics) were Linux-specific to begin
with. A soak run declares its host OS in its evidence bundle (§9) precisely so a
failure is never silently generalized past the platform it was actually observed
on. Extending real load injection to macOS/Windows is out of scope for v1 and is
not blocking for #1479/#1480/#1481.

## 4. Driver shape (#1479)

The driver runs **N concurrent real workflow runs against a real `goobers up`
daemon inside the isolated container**, not synthetic unit-level load. This
mirrors `test/scale/mixedload.go`'s own justification: using the real code paths
that create load-dependent failures, rather than an internal API call that
bypasses whatever made the original bug possible, "removes the defect from the
experiment designed to detect it." Concretely:

1. The container starts one `goobers up` daemon against a scratch instance
   (`bin/goobers init --demo` shape, no external credentials — mirroring the
   demo instance's mock-provider posture so the soak run needs no network writes
   and no live repository).
2. A small, checked-in **soak workflow fixture** (a new file under
   `test/soak/fixtures/`, not a shipped gaggle workflow) declares several
   deterministic stages whose `run.command`s do the SAME class of thing #811's
   trigger did: spawn a real subprocess, write real temp files, exercise real
   git/file-descriptor churn — enough to be a faithful current-day analog of
   "many real stages competing for CPU, disk I/O, and process/FD table slots,"
   without requiring `go test -race` itself (too slow to repeat at soak
   cadence; the fixture stages are a stand-in for **any** future gate that
   spawns real subprocesses under load, not a re-run of `make ci` specifically).
3. The driver triggers N concurrent runs of that workflow (`goobers run`, driven
   by the CLI against the live daemon — the same entrypoint a real trigger
   uses) and, independently, an **external load-injection process** in the same
   container applies the profile's CPU/memory/disk pressure for the sustained
   window (§6), so the daemon's own runs are contending against load that is
   not itself part of what's being measured — matching `test/scale`'s "hold
   everything else constant, vary only the one load axis" discipline.
4. The driver polls run state through `internal/readservice`'s existing query
   shape (`RunListOptions` filtered by `workflow`, `phase`, `since`/`until`) — the
   same interface the portal and CLI already use — rather than inventing a new
   observation path. This is a deliberate reuse, not a convenience: a soak
   harness that read its own private log format would prove nothing about
   whether the *product's own visibility* into a stuck run still works under
   load, which is itself part of what #811-class incidents degrade.

## 5. Admission ordering

The driver's own admission step is named, and behaves like, the scheduler's
existing one — `internal/localscheduler.Conditions.Admit` — deliberately, so this
document is not inventing parallel vocabulary for the same concept:

- Runs are **staggered, not admitted all at once.** The driver ramps from 1 to N
  concurrent runs over a fixed ramp window (default 30s per the profile — see
  §6), starting a new run only once admission for it succeeds. A burst of N
  simultaneous starts would conflate "can the daemon absorb a spike" with "can it
  sustain steady contention," and #811 was specifically a *sustained*-concurrency
  bug, not a burst one.
- Each admission attempt is either **admitted** (the run starts and its run ID is
  recorded) or **held**, and a held admission is retried on a fixed backoff until
  the ramp window's own deadline — never blocked indefinitely. This mirrors
  `Conditions.Admit`'s own contract: admission "never fails a tick... exhaustion
  means skip this tick, never an error."
- Every admission decision is reported with a **stable reason string**, in the
  same spirit as `Conditions`'s `Reason*` constants
  (`ReasonMaxParallel`, `ReasonBudget`, etc.): `soak: admitted`,
  `soak: held (ramp)`, `soak: held (instance max-parallel)`,
  `soak: refused (ramp deadline exceeded)`. The driver's own event log records
  these the same shape `tick.skipped` journal events already do (stable
  code, timestamp, run identity when one exists) — so a soak evidence bundle
  (§9) reads the same way a production journal excerpt does, no separate
  vocabulary for a human correlating the two.
- After the ramp completes, the driver holds exactly N concurrent runs for the
  sustained window (§6's `Duration`), starting a replacement run promptly
  whenever one finishes — steady-state contention, not a decaying burst.

This directly answers the prior review's "admission ordering... underspecified":
ramp-then-hold-steady, staggered starts, retry-with-backoff-until-deadline, and a
closed set of stable reason strings modeled on code that already ships.

## 6. Load profile and its transport

**Decision: a small typed Go struct with named presets, selected by CLI flag —
the same shape `test/scale`'s `LoadSpec` and `test/stress`'s `options` already
use.** This repository's existing harnesses configure themselves through Go code
and flags, not external YAML/JSON config files; introducing a new config format
for this one harness would be a second convention for identical problems that
`test/scale`/`test/stress` already solved.

```go
// test/soak/profile.go
type Profile struct {
    Name string // stable identifier, recorded in every evidence bundle (§9)

    // Concurrency (§4/§5)
    Runs       int           // steady-state concurrent workflow runs (N)
    RampWindow time.Duration // time to reach N from 1
    Duration   time.Duration // sustained window once N is reached

    // Injected resource pressure (§3), passed straight through to the
    // container's own resource-limit flags — never emulated in-process, so the
    // pressure the daemon experiences is the same pressure the OS scheduler and
    // filesystem actually apply, not a Go-level approximation of it.
    CPULimit        float64 // fractional CPUs, e.g. 1.5 -> --cpus=1.5
    MemoryLimitMB   int     // --memory
    DiskReadLimitMBs  int   // --device-read-bps
    DiskWriteLimitMBs int   // --device-write-bps
}

// Presets are named and versioned by name, not by field values, so a profile
// referenced from an issue or an evidence bundle (§9) stays meaningful even
// after its numbers are tuned.
var Presets = map[string]Profile{
    "smoke":    {...}, // seconds, low N — local `make soak-smoke`, a sanity check
    "standard": {...}, // the default nightly profile (#1481)
    "hostile":  {...}, // higher N, tighter resource caps — manual dispatch only
}
```

`test/soak`'s `main.go` takes `--profile=<name>` (default `standard`), resolving
to one of `Presets`; a profile name that doesn't exist is a hard, immediate
failure before the container even starts — the same "refuse before the expensive
thing happens" discipline `internal/readmodel`'s closed-set filter check already
uses. There is no free-form profile authoring in v1: every profile is a named,
reviewed, checked-in preset, so a soak run's parameters are always something a
reader can look up by name rather than reconstruct from a one-off flag
invocation. Ad hoc parameter overrides can be added later behind explicit flags
once there's a real need (#1479 may propose this); v1 does not need it and
naming a hypothetical need now would be exactly the kind of underspecification
the prior review already flagged once.

This directly answers "profile transport... underspecified": a typed Go struct,
a closed set of named presets, one `--profile` flag, no new config format.

## 7. Health signals — pass/fail

A soak run's outcome is **not** a single boolean. It reports, per run:

- **Throughput**: completed-run count over the sustained window, queried through
  `internal/readservice.ListRuns` filtered to `phase=completed` within the
  window — must stay non-zero throughout every rolling sub-window (default 60s;
  a zero-throughput sub-window anywhere in a sustained run is itself a failure,
  independent of the final count, because it is exactly the "wedged for a while
  then somehow recovered" shape a single end-of-run count would hide).
- **No infra-caused escalation**: zero runs reaching `phase=escalated` with a
  terminal cause naming an infrastructure failure (queried the same way
  #1405's own investigation queried run outcomes — `RunListOptions` filtered by
  `outcome`/`phase`) — the harness only fails on infra-shaped breakage, not on
  the fixture workflow's own intentionally-injected stage failures (the fixture
  from §4 needs a real failure path to exercise retry/failure code under load,
  and that failure must not count against the soak's own pass/fail).
- **No wedged runs**: zero runs still `phase=running` more than the fixture's
  own generous per-stage timeout past the sustained window's end — this is
  #811's own signature (a run that simply never finishes) and is checked
  explicitly, not inferred from throughput alone.

All three must hold for a soak run to **pass**. Any one failing is a **fail** —
reported with the specific signal that tripped, never collapsed to a bare
boolean, because #1481's failure report (§10) needs to say *which* contract
broke.

## 8. The invalid-result contract

Distinct from pass/fail: an **invalid** result is one that must never be counted
toward either, because the run did not actually exercise what the profile
claims to have exercised. This follows two existing patterns in this codebase
rather than inventing a third:

- `apiv1.ResultNoWork`'s discipline, stated in its own doc comment: "this status
  is only for 'correctly found nothing,' ... never a masked error." A soak run
  that could not even start (container failed to launch, the daemon failed
  health checks before the ramp began, the load-injection process itself
  crashed) is **invalid**, not a **pass** — an infrastructure precondition
  failure must never present as "nothing broke."
- `internal/readservice.StagePopulation`'s nil-vs-empty distinction: a run's
  throughput/escalation signals (§7) come back **absent** (not merely zero) when
  the harness's own observation path failed mid-run (the polling loop lost the
  daemon, the container was OOM-killed by the *host* rather than the profile's
  own memory limit — signaling the isolation itself broke, not the code under
  test). Absent is reported and treated as invalid; a genuine zero (queried
  successfully, actually zero) is a real failure under §7, never silently
  reclassified as "no data."

The closed set of invalid-result reasons, each a stable string in the evidence
bundle (§9):

| Reason | Meaning |
|---|---|
| `container-launch-failed` | Docker could not start the container with the requested resource limits |
| `daemon-health-check-failed` | `goobers up` never reached ready before the ramp deadline |
| `load-injector-crashed` | The external CPU/memory/disk pressure process exited before the sustained window ended |
| `host-oom-killed` | The container was killed by the **host's** OOM killer, not the profile's own `--memory` limit — evidence the isolation boundary itself leaked, not that the workload used too much memory on purpose |
| `observation-path-lost` | The driver's own polling against `internal/readservice` failed mid-run (network/daemon-API loss, not a run outcome) |

An invalid result **blocks** a pass or fail verdict for that run entirely (§10
still uploads its evidence, marked invalid) and, on the scheduled cadence
(#1481), is reported separately from both — see §11 — so a broken harness never
silently inflates a "passing" streak, and a string of `container-launch-failed`
results (an infrastructure problem, not a product regression) never trips the
same alert path as a real `phase=escalated` finding would.

## 9. Evidence on failure

On any **fail** or **invalid** result, before the container is torn down, the
harness captures and uploads (mirroring `test/stress`'s and `test/scale`'s
existing `if: always()` artifact-upload pattern):

- the profile name and full resolved `Profile` struct (§6);
- the container's resource-limit flags as actually passed to Docker (so a
  limit that silently failed to apply is visible, not assumed correct);
- every admitted run's ID, admission reason history (§5), and terminal phase;
- for any run matching a §7 failure signal: its full journal (`events.jsonl`),
  the same artifact class a human already knows how to read from `goobers
  trace`;
- a goroutine dump of the daemon process (`SIGQUIT`, the same technique that
  actually found #811/#845's real cause) taken automatically the moment a
  wedged-run signal fires — not left to a human to remember to capture live,
  which is exactly what made #811's original diagnosis take two attempts;
- host OS, container runtime version, and the git revision under test.

Evidence upload happens for **invalid** results too (§8) — an infrastructure
failure that leaves no trace is just as unresolvable as a code failure that
leaves no trace, and #1481's own health depends on distinguishing "the harness
itself keeps breaking" from "the harness is fine and keeps finding real bugs."

## 10. Reporting without alert noise (#1481)

The scheduled soak workflow does **not** invent a new reporting path. It reuses
`test/stress`'s already-shipped shape exactly:

- Trigger: `schedule:` (nightly cron) + `workflow_dispatch:` — never
  `pull_request:`/`push:`/`merge_group:` (§11's "never blocks PR CI" is enforced
  structurally here, not by convention).
- `concurrency: {group: soak-<ref>, cancel-in-progress: false}` — matching
  `stress.yml` exactly, so an overlapping manual dispatch queues behind a live
  scheduled run rather than clobbering its evidence mid-capture.
- On a **fail**, and only on `github.event_name == 'schedule'` (never on manual
  `workflow_dispatch`, which is how a human investigates without generating a
  duplicate alert for the same run they're already looking at): route the
  failure into `test/flakeledger`'s existing issue-filing path — the same
  already-designed, already-deduplicating, already-noise-bounded flow
  `stress.yml`'s `flake-ledger` job uses. This is a deliberate reuse: building a
  second issue-filing/deduplication mechanism for a second kind of "test found
  something real" event is exactly the kind of parallel infrastructure this
  design avoids elsewhere (§4, §6).
- On **invalid** (§8): a *separate*, lower-severity report — a comment on a
  single rolling tracking issue (one issue, updated in place, never a new issue
  per occurrence) rather than `flake-ledger`'s per-finding issue creation. An
  infrastructure hiccup is operationally different from a product regression and
  must not compete for the same triage attention; three consecutive `invalid`
  results (not `fail`) escalates that tracking issue's label to
  `goobers:needs-human`, since a harness that cannot run three nights running is
  itself the more urgent problem.
- On a clean **pass**: no report at all. Silence is the steady state; alert
  volume scales only with actual findings, matching `stress.yml`'s own posture
  (its `flake-ledger` job runs unconditionally but only *files* when its input
  actually contains a failure).

## 11. Budget, cadence, and the local/scheduled boundary

| Profile | Where it runs | Cadence | Budget |
|---|---|---|---|
| `smoke` | Local (`make soak-smoke`), `workflow_dispatch` | On demand | ≤5 minutes — a sanity check a change author can run before opening a PR, never inside `ci.yml` itself |
| `standard` | `.github/workflows/soak.yml`, scheduled | Nightly | ≤60 minutes, mirroring `stress.yml`'s 90-minute ceiling scaled down for a smaller N |
| `hostile` | `workflow_dispatch` only | Manual, e.g. before a scheduler/executor-touching release | ≤90 minutes |

None of these profiles, at any cadence, run inside `ci.yml`'s `pull_request:`- or
`merge_group:`-triggered jobs — the soak workflow is its own file
(`.github/workflows/soak.yml`), triggered exactly as `stress.yml` is (§10), so a
soak run can never be a required check and can never add latency to a PR merge.
This is the same boundary `test/stress` and `test/scale` already established for
their own nightly/weekly tiers; this design draws the identical line rather than
re-arguing it.

## 12. Decomposition

| Issue | Owns | Depends on this doc for |
|---|---|---|
| #1479 | The concurrent-load driver: admission ramp/hold (§5), the soak workflow fixture (§4.2), throughput/escalation/wedge health signals (§7) | The exact admission reason strings, the fixture stage shape, and the pass/fail signal definitions — all fixed here so #1479 has no open design questions of its own |
| #1480 | The isolated execution environment: the Docker image, resource-limit flag wiring, `host-oom-killed` detection (§8), evidence capture (§9) | The chosen isolation strategy and its portability limits (§3), and the exact evidence bundle contents (§9) |
| #1481 | The scheduled workflow goober: `.github/workflows/soak.yml`, profile selection (§6), `flake-ledger` integration and the rolling invalid-result tracking issue (§10) | The profile preset shape (§6), the pass/fail/invalid three-way split (§7/§8), and the exact reporting rules (§10) |

Each can proceed independently once this document lands: #1479 needs no
knowledge of Docker specifics, #1480 needs no knowledge of the fixture
workflow's stage contents, and #1481 needs no knowledge of either beyond the
`Profile` struct and the pass/fail/invalid contract.

## 13. Non-goals (v1)

- Real resource-pressure injection on macOS or Windows (§3) — Linux only.
- Ad hoc profile authoring outside the checked-in preset set (§6).
- Catching every possible load-dependent bug class. This harness's fixture
  stages (§4.2) exercise process/subprocess/disk/FD contention, the shape #811
  actually was. A load-dependent bug in a completely different subsystem (e.g. a
  race specific to the portal's SSE fan-out under many connected clients) needs
  its own fixture eventually, not a claim that this one already covers it.
- Micro-VM isolation. Named in §14 as a reconsideration trigger if Docker's
  isolation proves insufficient (a soak run's own resource limits leaking onto
  the host, or a false `host-oom-killed` rate high enough to make the harness
  itself unreliable) — not committed to now.

## 14. Reconsideration triggers

Revisit this design if any of the following happens:

- Docker's resource limits prove leaky in practice (§13) — move to micro-VM
  isolation.
- A load-dependent bug is found that this harness's fixture (§4.2) structurally
  cannot reproduce (e.g., needs many real network peers, not just local resource
  contention) — that is a new fixture, and possibly a new profile axis, not a
  reason to abandon the driver/admission/evidence contracts fixed here.
- The `invalid` rate on the `standard` nightly profile stays persistently above
  a small fraction of runs — that means the harness's own infrastructure is the
  unreliable part, and #1480's environment needs hardening before the `fail`
  signal (§7) can be trusted at all.
