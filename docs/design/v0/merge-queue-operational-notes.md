# Merge queue — operational notes (prep draft)

> Status: **Draft — prep slice, not the full #631 design.** Written ahead of the queue
> actually being enabled (that changeover is #759, separate and PM-executed) and ahead
> of #758 (merge-policy abstraction), which `merge-review`/auto-merge will consume to
> become queue-aware. Terminology here should be reconciled against #758's once that
> design posts — treat this as a placeholder for the real runbook section #631's own
> acceptance criteria calls for, not that section itself.
> Related: [`pr-lifecycle-loop.md`](pr-lifecycle-loop.md) §7 (Auto-merge & trust) ·
> [`../validation-and-ci-enrichment.md`](../validation-and-ci-enrichment.md) §3 (CI2)

## Why this exists

Issue #631 (adopt GitHub merge queue on `main`) closes G4, the sibling-collision
incident class (#377 + #378: two PRs each green in isolation, mergeable individually,
but never built together — the second one broke main on merge). A merge queue closes
this by validating every merge against the *projected* post-merge main (current main +
everything already ahead in the queue + this PR), not just the PR's own stale base.

This note is scoped to the one thing that's safe to describe before the queue exists:
what changes for anyone watching a PR once it does. It is not a queue admin guide (that
lives with #759's settings changeover) and it does not describe `merge-review`'s
eventual enqueue-mode behavior (that's #631's main body, blocked on #758).

## What changes for a PR once the queue is live

Today, "CI green + approved" on a PR means it's mergeable. Under the queue, it means
the PR is *eligible to be enqueued* — merging still requires GitHub to actually add it
to the queue (manually, or by whatever automation #631/#758 wire up) and for the
queue's own build to pass.

- **Enqueued.** GitHub creates a temporary merge commit — main plus every PR already
  ahead of this one in the queue, plus this PR — on a synthetic ref
  (`refs/heads/gh-readonly-queue/main/pr-<number>-<sha>`), and reruns the required
  check set (`.github/workflows/ci.yml`'s `merge_group` trigger) against it. The
  originating PR shows a queued/pending state; the actual build runs as a check on the
  synthetic ref, not as a new run on the PR's own branch.
- **Merged.** The queue build passed. GitHub fast-forwards (or squashes, per branch
  protection's merge-method restriction) the change onto `main`. From this point on,
  everything downstream (issue close-out, sibling `needs-remediation` fan-out,
  telemetry) behaves exactly as it does for a direct merge today — the queue only
  changes how the merge gets validated, not what happens after.
- **Evicted.** The queue build failed, *or* an earlier entry in the queue was evicted
  and every entry behind it must be re-validated from a clean base. GitHub removes the
  PR from the queue and reports the failure on the synthetic ref's check run, not
  necessarily as a fresh failure on the PR's own last-known-good CI run — a human or
  agent consumer checking only the PR's own status checks can be misled into thinking
  it's still green. **Until #631's queue-aware workflow wiring lands, nothing in the
  agent loop watches for this distinction** — an evicted PR just sits enqueue-eligible
  again, unpicked, until whatever re-triggers selection notices it's still open. This is
  the concrete gap #631's acceptance criteria ("surface queue eviction as an actionable
  outcome") exists to close.

## Open questions for #631's real runbook section

These are flagged, not answered, here — they belong to #631's own landing once #758
exists:

- How does an agent (not a human watching the GitHub UI) *observe* enqueued vs. merged
  vs. evicted state? The REST/GraphQL surface for merge-queue entries is not the same
  as PR mergeable-state; whatever `merge-review`/`pr-remediation` end up polling needs
  to resolve this against #758's `Land(pr)` abstraction, not invent a second query path.
- What does `merge-review`'s verdict writeback say once "pass" means "enqueued" rather
  than "merged"? (#631 AC: "distinguish enqueued vs merged in their writeback.")
- Batch/rebase strategy tuning for queue throughput is explicitly out of scope for
  initial adoption (defaults first, tune only on observed contention) — not addressed
  here either.
