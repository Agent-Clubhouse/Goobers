# Sibling serialization is a workflow choice: built-in ordering strategies, with election as the opt-in

Suggested labels: area:runner, area:dsl, type:enhancement, goobers

## Problem

Overlapping open PRs need a serialization mechanism or merge-review
deadlocks: reviewers can only block each PR on its siblings (observed live:
19 open PRs, dense file overlap, zero merges for hours — every verdict
"needs-changes" with ordering-asks misclassified as substantive, because no
better vocabulary existed). The product currently offers exactly one answer
— the election machinery (elect-lander/elect-gate, blocked-on-sibling
labels, sticky-comment payloads, demotion counters, unpark actions) — which
is powerful but heavy: its state lives in provider labels other lanes can
clear (an observed label ping-pong erased review signals three times on one
PR), it has known deadlock modes (info-severity findings), and a minimal
gaggle that "deliberately drops" it inherits the deadlock instead.

## Evidence

- coldstart + live incident: docs/audits/2026-08-08-gaggle-reliability/
  (zero-merge deadlock diagnosis; PR #226 label ping-pong timeline; PR #237
  with 10 overlapping siblings and hasSubstantiveFindings=false).
- The fix that unjammed the live site gaggle: a 15-line deterministic script
  stage — "lander = lowest-numbered open member of the overlap set; others
  finish quietly before review" — instance commit 270fd16. State recomputed
  from the live PR set per run: nothing to leak, stick, or unpark.
- cmd/goobers/applyverdict.go:61-73 (verdictLabel), #747 (cross-pr-blocked
  split), the election-deadlock history.

## Proposed direction

Operator-ratified framing: **both, opt-in.** Three tiers:

1. **Built-in `siblingStrategy` on the selection path** (pr-select or
   gather-sibling-context input): `oldest` — FIFO by PR number; only the
   current lander of an overlap set proceeds to review, reviewed on its own
   merits; siblings drain in order as landers merge — and `none`. Smart
   default: `oldest` for new scaffolds; predictable, zero agent cost, no
   provider-side state. The runtime already computes the overlap set; the
   strategy is a comparison.
2. **Election stays as the shipped composable flow** for gaggles whose
   throughput justifies judgment-based lander choice (a big/slow eldest PR
   should not always head the queue). A workflow opts in by carrying the
   elect stages, exactly as today — but as a declared choice against the
   default, not as the only mechanism.
3. **Custom strategies are script stages** (first-class-scripts direction):
   priority labels, size-ascending, milestone-first — the live fix is the
   existence proof that a bespoke strategy is ~15 lines.

Reviewer contract consequence (all tiers): when a serializer upstream has
already chosen the lander, `cross-pr-blocked` narrows to true content
dependencies; mere file overlap is never a finding. This simplifies the
verdict vocabulary for every strategy including election.

Zero-config behavior: existing workflows unchanged (`none` semantics until
they declare a strategy or carry elect stages); new scaffolds get `oldest`.

## Alternatives considered

- Elections as the only mechanism (status quo): minimal gaggles inherit a
  deadlock when they drop it; the machinery's provider-label state model is
  the observed ping-pong source.
- FIFO as the only mechanism: throughput-hostile at scale; a slow eldest PR
  heads the queue indefinitely.
- Merge queue: previously removed for this instance's scale; a fourth tier
  for repos that have one, not a replacement for selection-side ordering.

## Duplicate search

2026-08-09: searched election, elect-lander, sibling ordering, serialize
PRs, blocked-on-sibling, lander strategy (open+closed). #747 (cross-pr-
blocked split), #1855, #950 are patches to the election flow, not a
strategy surface; no issue proposes selection-side ordering or a strategy
enum. The fan-out ergonomics drafts in this directory are orthogonal
(branch-level verdicts, not sibling ordering).

## Size and risk

M. Strategy `oldest`/`none` is selection-side logic + one input + scaffold
default; election tier is unchanged code, re-documented as opt-in. Risk:
low — additive, default-compatible; the reviewer-contract narrowing needs
one instructions/docs pass on shipped reviewer personas.
