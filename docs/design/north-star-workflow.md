# North-star workflow — the runtime's acceptance test

**Status:** Draft for backlog planning (PO-directed, 2026-08-12). Documentation-first:
this doc plus the capability epics filed from it are the deliverable. No implementation
is scoped here.

**Purpose.** Goobers' V2 runtime gaps are currently a list — task queues, platform
routing, parallel branches, projection. A list has no ordering principle and no cut
line. This doc states **one workflow we want to be able to run**, and every capability
gap below is justified by a step of it. Anything the north star does not require does
not get built; anything it does require gets filed with a reason.

This workflow is deliberately *not* a spike rung. The Goobernetes ladder (#2838) proves
the cloud shape with simple workflows. This is what the runtime should be able to
express once the ladder's exhaust has been paid down.

---

## 1. The workflow

```
  [linux]  deterministic scripts
     │
  [linux]  implement            (agentic)
     │
  [linux]  goober review        (agentic)
     │
     ├──────────────┬──────────────┐   TRUE PARALLEL — same commit
  [linux] CI    [windows] CI       │
     │              │              │
   fail?          fail?            │   conditional, per leg
     │              │              │
  [linux]        [windows]         │
  remediate      remediate         │   agentic, may fire 0, 1, or 2
     │              │              │
     └──────┬───────┘              │
        merge step                 │   agentic — only when BOTH fired
            │                      │
            └──── JOIN ────────────┘
                   │
          ┌────────┴────────┐
     0 legs fired      ≥1 leg fired
          │                 │
          │            goober review     (once, not once-per-leg)
          │                 │
          └────────┬────────┘
                   │
              local CI                   (workflow-defined; today: the full set)
                   │
              full pass ⇒ open PR
```

## 2. Semantics (PO rulings, 2026-08-12)

| # | Question | Ruling |
|---|---|---|
| 1 | Review once or once-per-leg on the join? | **Once.** If both legs fired, an agentic **merge step** reconciles the two remediation branches before review sees them. |
| 2 | Zero legs fired? | **Skip the repass entirely** — straight to local CI. No review re-run. |
| 3 | What is "local CI"? | Whatever the workflow declares. For the Goobers gaggle today that is the full set; it is not a distinct primitive. |
| 4 | Why platform-matched remediation? | If CI is solid and the failure is platform-specific, the agent has better odds fixing it **on that platform**, where it can see and test the real thing. Possibly contrived; kept because the reproduction environment is the point. |
| 5 | Cycle bound? | **Unchanged repass semantics.** A budget on the back edge, possibly very high or effectively unbounded. Naive **shared** repass across the whole cycle is fine — it does not reset when leg A fails, then leg B. |

**The hardest requirement is item 1's precondition:** the parallel branches *edit*. Two
remediation agents mutate the same working copy concurrently, which is why a merge step
exists at all. Parallel read-only fan-out is a much smaller problem than parallel
fan-out with write access, and this workflow requires the latter.

## 3. Capability gaps

Each row is a thing the north star needs that does not exist today. Current-state
claims were verified against `origin/main` @ `aac3a3d4` on 2026-08-12; re-verify before
implementing, as file:line drifts.

### 3.1 Stage placement — declare / admit / route / fail

The single largest gap, and it is one problem with four faces. Today the same line of
YAML means three different things depending on which path executes it.

| Face | Current state |
|---|---|
| **Declare** | `RequiredCapabilities` is per-stage in the DSL but is **unioned into one per-run set** by `instance.WorkflowRequiredCapabilities` before anything reads it. The union's own doc comment states the assumption: *a run executes all of a workflow's stages on one runner.* You can say "this workflow needs Windows"; you cannot say "this stage needs Windows." |
| **Admit** | Tier 2 unions across every workflow bound to a gaggle and checks at **daemon startup** — a mismatch is a hard startup failure, not a skipped run. The engine path reads capabilities **not at all**. |
| **Route** | `engine.NewTemporalStarter` takes exactly **one** task queue. The consumer half is ready — `goobers worker --task-queue` is repeatable — but the producer half cannot address more than one queue. |
| **Fail** | Tier 2 fails fast. The engine path sets no `ScheduleToStartTimeout` on stage activities and no execution/run timeout on the workflow, so an unroutable stage **queues and waits indefinitely**. |

`docs/design/mixed-platform-cloud-nodes.md` (#659) describes per-stage platform routing
as though it exists. It does not. That doc needs a correction alongside this work.

**Scope note (PO, 2026-08-12): this is bigger than platform routing.** Task queues and
concurrency should be rethought together, including limits at **workflow, gaggle, and
instance** scope and the **dequeue behavior** at each. Today's ceilings
(`maxParallelRuns`, per-workflow `maxConcurrentRuns`/`maxRunsPerHour`,
`claimsLockTimeout`) were designed for one machine draining one queue.

*Needed by:* the parallel CI legs, both remediation branches, and every mixed-OS step.

### 3.2 Parallel branches that edit, with conditional fan-in

Tier-3 DSL extensions — parallel branches and child workflows — are filed as #155 and
explicitly deferred behind the sequential path. The north star needs more than #155 as
written:

- branches that **mutate a working copy**, not just read;
- a **conditional** fan-in that handles 0, 1, or 2 completed legs;
- an **agentic merge step** that runs only in the both-fired case;
- a **backward edge** from the join to an earlier stage, under a shared repass budget.

`docs/design/static-fan-out-fan-in.md` is the nearest existing design and does not cover
the editing or backward-edge cases.

*Needed by:* the parallel CI legs, remediation, merge, and the review back edge.

### 3.3 Artifact and transcript projection

At tier 1 the journal is written live by a journal-bound recorder inside each executor.
On the engine path there is no journal at activity time, so the journal is reconstructed
afterward from Temporal history — and history carries only what the workflow itself
authored. Consequences:

- **Transcripts cannot be represented at all.** The projection whitelist has nine event
  types and excludes `journal.EventSpanRecorded`; `JournalOp` has no span kind, so a
  transcript fails closed as `ErrUnprojectable`.
- **Stage-produced artifact bytes have no channel.** The only artifacts the engine
  carries are context manifests and gate verdicts; stage artifacts appear as bare refs
  whose blobs were never written.
- **Projection is not idempotent** — `journal.Create` hard-fails if the run directory
  already exists, with no retry path.

Recommended direction: executors write artifacts and transcripts **directly to shared
journal storage at execution time**, as tier 1 does, and the projection reconstructs
only orchestration events. This matches `docs/design/k8s-infra-shape.md` §4 — shared
storage is a projection target, not a coordination mechanism.

*Needed by:* every step. A workflow this shaped is undebuggable without transcripts, so
this is a prerequisite rather than polish.

### 3.4 Observability and the cloud visibility story

Raised by the PO alongside the above, and currently unscoped:

- More logging generally, and specifically around Temporal.
- **Correlation between Goobers' own logs and Temporal's** — via Temporal memo,
  search attributes, or another decorator — so the two join cleanly or auto-enrich
  rather than being read side by side.
- An explicit decision on the cloud visibility surface: **portal still? Temporal UI
  only? portal plus Temporal?** Today the portal is the only run-reading surface and it
  is served by a separate command (`goobers dashboard`), not by `goobers up`.

*Needed by:* operating any of this in a cluster.

### 3.5 Claims off files

Not required by the north star's *shape*, but required by its *deployment*: the claim
ledger and its siblings coordinate through `flock(2)` (one primitive,
`internal/platform/lock`, 14 non-test acquisition sites), which is node-local by
construction. Parallel branches on different nodes cannot share it. Tracked separately;
see the claims-migration epic.

## 4. Explicitly out of scope

- Any implementation sequencing. This doc justifies work; it does not order it.
- The Goobernetes spike ladder (#2838), which needs none of the above and should not
  wait for it.
- Whether this exact workflow ever ships as a shipped gaggle workflow. It is a target
  to design against, not a product commitment.

## 5. Open questions

1. Does the merge step need its own capability/credential scope, or does it inherit the
   remediation agents'?
2. Is the repass budget owned by the reviewer gate, or by the cycle as a whole? The PO
   leaned toward the reviewer gate's existing repass but flagged it as undecided.
3. If a remediation leg exhausts its budget, does the run abort, escalate, or open the
   PR with a known-failing platform noted?
4. Do both CI legs run against the same commit, or does each leg push a platform fixup
   first? The diagram assumes the same commit.
