# Distributed state and coordination

**Status:** draft, written from field evidence on the Goobernetes spike branch
**Scope:** what has to change for a Goobers run to be executed by more than one process on more than one machine
**Not in scope:** scheduling policy, concurrency limits, autoscaling. Those are downstream of this and get harder to design before it lands.

## Why this document exists now

An OS-spanning run works. On 2026-08-12 run `300534f6f9503e251374d9433060ebf8` executed three stages across a Linux node and a Windows node in one AKS cluster, on one workflow definition, with one run id — dispatched by Temporal, with stage artifacts exchanged through a shared content-addressed store.

That was not supposed to be possible yet. The mixed-OS rung was parked behind "claims off files" on the assumption that distribution needed the claims migration first. It did not, and understanding *why* is the whole basis of this design:

> **A worker is not an instance.** It owns no claim ledger, mints no runs, serves no API. It polls a queue, provisions a workspace, executes one stage, and returns a result envelope.

Everything that is hard about distributing Goobers is hard because it lives on the *instance* side of that line. Everything that turned out to be easy was already on the worker side. So the useful question is not "how do we distribute Goobers" but "what is still on the wrong side of that line, and what is each thing costing."

## The line, drawn precisely

A stage executing somewhere else needs four things. Three of them already work.

| # | What a remote stage needs | Mechanism | State |
|---|---|---|---|
| 1 | To be told to run | Temporal task queue, per **activity** (`ActivityOptions.TaskQueue`) | **works** |
| 2 | A place to work | fresh worktree per attempt, provisioned from the repo, discarded after | **works** |
| 3 | Its predecessor's outputs | `ResultEnvelope` through Temporal history | **works** |
| 4 | Its predecessor's *artifacts* | `cp.Artifact.Resolve(journalRoot)` — a **local filesystem path** | needed the store below |

(4) is worth dwelling on because it is the template for everything left. It was not a missing feature. It was a *correct* implementation of an assumption — one runner, one directory — that stops being true the moment there are two. The failure mode was not an error message about distribution; it was a missing file, reported as an integrity fault, from code that was behaving exactly as designed.

Every remaining item on the list has that shape.

## The pattern: identity without location

The fix for (4) generalises, and it is the single design idea in this document.

`journal.Ref` was already a sha256 of the scrubbed bytes. That means a blob's **identity is already independent of where it lives** — which is the one property a distributed data plane needs, and it was sitting there unused. So the node-local run directory becomes a *cache* in front of a store the fleet shares: write through on record, fetch any digest not held locally before the stage runs. `Resolve` still reads a local path. `journal.Run`, the projection, and the entire local runner are untouched. An instance with no store configured behaves byte-for-byte as before.

Two properties do the real work, and both are worth stating as design rules rather than implementation details:

**Idempotent writes replace locks.** `Put` of a digest already present is a no-op, published by rename. Two workers racing on one digest are, by construction, writing identical bytes — so the write is made *idempotent* instead of *serialized*. That is flock's job, obtained without flock.

The operational consequence is sharp: the store is safe on precisely the storage a journal is not. Azure Files has no dependable POSIX locking, which is why blessing it as rwx-capable is a false green for the journal and the claim ledger (#2854) — those coordinate *by locking a shared file*. A content-addressed store never locks, so the same share is entirely sound for it.

**Names that arrive over the wire are inputs, not paths.** A digest reaches the fetch layer from a `ContextPointer`, which travels through Temporal history from an upstream stage. An unvalidated one is a path traversal with a remote origin. Validate before it becomes a path — the same reason `ValidRunID` exists, applied to digests.

Generalised: **make the thing addressable by identity, make the write idempotent, and the coordination problem disappears instead of moving.** Where that is not possible, you need a real coordinator — and the point of this document is that we should know which of those we are choosing, each time.

## What is still node-local, and what each one costs

Six mechanisms, in the order they will bite.

### 1. Code state lives in the worker's git mirror

Every stage attempt gets a fresh worktree on the run branch and discards it when the stage ends, so nothing survives on the filesystem between stages *by design* — good. What survives is the **branch**, in that worker's mirror clone. On one node this is invisible and free: the agentic stage commits, and the next stage's fresh worktree sees the commit because the mirror is the same one. On another node the mirror is a different clone and the commit is simply absent.

**Cost:** a stage that hands work to another platform must push first.
**Fix:** no new mechanism — a rule the DSL cannot currently state and the compiler could check. A platform hop is a push boundary. Much better said by the compiler than discovered by an adopter whose Windows stage committed into a mirror nobody else can see.

### 2. The claim ledger is per-node

Confirmed by construction on the OS-spanning fleet: each worker mounts its own volume, so `claims.json` (and `claims.lock`, and every other instance state file) exists **twice**, diverging silently. Today's workflows are safe only because the one claims-mutating stage — `backlog-query --claim` — happens to be pinned to the Linux side.

**Cost:** claims-mutating stages are currently **single-placement by necessity, and nothing in the DSL says so or checks it.** Add a second worker on the same queue and two nodes claim the same backlog item, each unable to see the other's ledger. `flock` on a local file excludes nothing across machines.
**Fix:** this is the one that genuinely needs a coordinator. A claim is a mutually-exclusive lease over an external identity (an issue, a PR) — it has no content-addressable identity to exploit, and two claimants must actually be arbitrated. Options in increasing order of what they buy: a shared SQLite (still needs real locking, so it inherits the storage-class problem), Postgres (already running in-cluster for Temporal), or Temporal itself as the lease authority via workflow liveness. Postgres is the pragmatic answer; the Temporal option is interesting because a claim's natural lifetime *is* a workflow's.

### 3. Triggers arrive by file drop

External processes talk to a running daemon by writing a request file into `<SchedulerDir>/pending-triggers/`, which the daemon polls. Demonstrated live during this spike: every run was fired by `kubectl exec` **into the same pod**, and worked only for that reason. From another pod it would have written a trigger nobody sweeps.

**Cost:** no trigger, HITL gesture, or external integration can reach the daemon from anywhere else.
**Fix:** a write API. Four consumers want the same primitive — HITL escalation resolution, remote executors recording into a journal they do not own, external triggers, and the local seam itself (which gains auth, bounded latency, and a reply). Design it *with* the claims work, not after: the claims ledger is plausibly just the first table behind it.

### 4. Journal authorship is an in-process interface

The recorder is a Go interface holding a `*journal.Run`. A worker cannot author the journal for a run it did not mint without inventing conformance-normative identity fields, and `journal.Create` fails closed on an existing run directory.

**Cost:** this is why a worker stages artifacts rather than journaling them, and why the projection has to author the journal afterward from history.
**Nuance worth keeping:** `journal.Recover` already exists and is exactly the attach-and-append primitive a second process needs. It is named for crash recovery, which is why nobody had reached for it. It takes the per-run lock for the handle's lifetime — so it is correct for two processes *on one node*, and inherits (2)'s problem across nodes.

### 5. `up.lock` assumes one daemon, and two files depend on that silently

`trigger-evaluations.json` and `schedule-demand.json` have no lock and no generation check. Their safety is entirely inherited from the single-daemon guarantee, and nothing in either file's code says so.

**Cost:** the coupling is invisible, so the first attempt at a second daemon corrupts them without any code looking wrong.
**Fix:** whatever backs (2) should back these. Alternatively, make the dependency explicit *now*, cheaply, so a future change trips a guard instead of a data race.

### 6. Workspace locality is bound to local PID identity

Which breaks between two *pods on one node*, not just across machines — PID namespaces collide.

**Cost:** low today, and a genuine trap the moment more than one worker runs per node.

## Sequencing

The ordering follows from cost, not from difficulty.

1. **The compiler rule for (1).** Cheap, prevents a silent wrong answer, and is needed by the first real cross-platform workflow rather than the tenth.
2. **The write API and the claims store, designed together.** (2) and (3) are the same problem wearing different clothes, and (4) is the same seam again from the journal's side. Designing them separately produces three coordinators.
3. **(5) and (6) fold into whatever (2) lands as**, or get explicit guards in the meantime.

Two things deliberately come *after*, not before: scheduling across nodes, and concurrency. Both are policy over a coordinator that does not exist yet. Building them first means building them twice.

## The falsifiable claim

If this design is right, distribution should keep arriving in the same shape it just did: something that looked structural turns out to be an assumption, and the fix is to give the thing an identity that does not depend on where it is.

Two of the three "structural blockers" hit on this spike were already-existing capabilities nobody had used — `journal.Recover`, and Temporal's per-activity task queues. Both were reasoned around rather than looked up. That is worth treating as a prior: **when a limitation looks structural, check whether the substrate already has the feature before designing around its absence.**

## Evidence

Everything above was observed rather than reasoned. Filed upstream:
#2854 (storage preflight false green), #2860 (single-runner capability admission),
#2861 (two of three inter-stage channels are node-local), #2862, #2863, #2864
(mixed-fleet portability), #2865 (no tier-3 telemetry).
