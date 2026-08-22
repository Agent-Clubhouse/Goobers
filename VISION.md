# Vision: what "done well" looks like

This branch is not a proposal to merge. It's a target. `main` is where the
instance actually runs today — read it to see current reality, warts
included. This branch is where we write down, in the same YAML/Markdown the
instance is actually made of, what we'd write if we paused, forgot every
workaround we'd already shipped, and asked only: *what do we actually mean?*

If you're an agent working a loop task against this repo: `main` is your
starting point, this branch is your compass. Where a file here differs from
its `main` counterpart, the difference is the point — close it if you can
without inventing DSL surface Goobers doesn't have; if you can't close it
without new DSL surface, that's not a dead end, it's a well-scoped ask (see
"Needs upstream" below) — bring it back as a proposal rather than silently
reverting to the workaround. Where a file here has *no* `main` counterpart
change at all, that's also signal: it means we looked and didn't find
anything worth wanting differently.

## What "humming" means

A workflow is humming when:

- **A human can read it once and trust it.** Not "trust it because it's been
  running for months without complaining" — trust it because the shape is
  obviously correct: the control flow matches the goal in one read, the
  failure modes are visible in the branches, and nothing is quietly relying
  on a runtime quirk that isn't written down anywhere a reader would see it.
- **A mistake is caught before it runs, not after it's caused damage.**
  `goobers validate` should refuse a config that can't work, not just one
  that's syntactically wrong. A capability grant that resolves to a token
  with no access to the target repo, a budget input a stage will
  unconditionally require but the workflow never sets, a cap that promises
  per-gaggle enforcement it structurally cannot deliver — these should be
  validation errors, not production 403s and 404s discovered by an operator
  reading a run journal.
- **Shared shape is actually shared, not copy-pasted and hand-synced.**
  When two gaggles need the same role — dedup-vs-live-backlog, judge trust,
  file — that role should be defined once and referenced twice, with only
  the genuine differences (label vocabulary, repo identity) declared as
  parameters. Today it's defined twice, by hand, and drifts the moment one
  copy gets a fix the other doesn't.
- **The data crossing a stage boundary has a shape, not just a filename.**
  An artifact handed from a shaping stage to a filing stage should have a
  schema the runtime can check — this one child item's title/body/evidence,
  not "trust the prose instructions to notice if a whole plan got pasted
  into one issue body." Correctness of structure should not depend on an
  LLM reading 40 lines of defensive prose correctly every single time.
- **Config surface tells the truth.** If a field exists, it does what its
  name says, for every gaggle/workflow that can set it — not just the one
  that happened to be built and tested first.
- **Deviation is a choice, not an accident.** Two pipelines that do
  genuinely different things should look different. Two pipelines that do
  the same thing should look the same — and today, whether they do depends
  on which one got copy-pasted from which, not on a real design decision.

## Comments: intent, not incident

Every comment in this branch answers "what does this mean and why does it
need to be true," never "here's the bug we hit and the PR that patched it."
`main`'s comments are full of the latter, honestly, because that's how the
config actually got built — under real production pressure, discovering real
gaps, patching them as found. That history has value, and it isn't lost —
it's in `git log` and in the issues filed against Goobers, which is exactly
where "how did we get here" belongs. It doesn't need to also live forever in
the file a future author has to read just to understand *what the config
currently means*.

So: if a comment in this branch references an issue number, a date, a past
failure, or "we used to do X until Y happened" — that's a bug in the
document, not a feature. Rewrite it as a direct statement of the constraint
and, where genuinely non-obvious, the reason the constraint exists — dropping
the story of how we found out the hard way.

## The concrete wishes

Each of these replaces a real workaround shipped in this instance today.
Marked **today** where we can already write it (just haven't, uniformly) and
**needs upstream** where Goobers itself would need new DSL/runtime surface.

### 1. Structured artifacts (needs upstream)

A stage that hands off a "plan" or a "findings list" should declare the
*shape* of that handoff — a schema for one item (title, body, evidence,
severity, whatever the domain needs) — and the runtime should validate each
unit crossing the boundary against it, the same way `expectedOutputs`
already validates scalar outputs today. The filing stage should never need
defensive prose asking "is this actually one item, or did you hand me the
whole container by accident" — that should be a validation failure before
the filing stage ever sees it.

### 2. Shared, referenceable roles

Two different shapes of this wish, at two different scopes:

- **Within one gaggle: already achievable today, just not done uniformly.**
  A lens goober that runs N times in one workflow, each time with a
  different focus, is just one goober plus a per-branch input — no new DSL
  needed. `upstream-sync`'s `change-area-researcher` already does this;
  `quality-sprint` used to fork eight nearly-identical dedicated goobers
  (`test-coverage-researcher`, `cross-platform-researcher`, ...) instead —
  now collapsed into one `quality-lens-researcher`, parameterized by
  `areaName`/`areaFocus`, in this branch. Same pattern, same payoff: one
  instructions file to maintain instead of eight, per-branch behavior driven
  by inputs instead of by which fork you're editing.
- **Across gaggles: needs upstream.** A goober that's genuinely the same
  role in *every* gaggle that needs it — "the constant interface between a
  shaping stage's output and the live backlog: dedup, judge trust, file" —
  should be defined once, at whatever scope makes it shared (instance-level,
  or an explicit `extends`/`uses` reference), with per-gaggle differences
  (label vocabulary, trust-label prefix, backlog identity) declared as
  parameters. Today `backlog-clerk` (the `goobers` gaggle) and
  `site-sync-clerk` (`goobers-site`) are the same role, hand-forked and
  hand-kept-in-sync, because there's no cross-gaggle equivalent of the
  within-gaggle pattern above.

### 3. Capability grants that are inspectable and honest (needs upstream)

Today, whether a declared capability resolves to the right credential for
the right repo is invisible from the workflow/gaggle YAML — you have to read
the runner's credential-scoping source to know. `goobers validate` should
be able to answer, for any gaggle/capability pair, which token backs it and
whether that token can actually reach the gaggle's target repo — and should
refuse to validate a config where an instance-level credential override is
ambiguous across gaggles with different target repos, rather than silently
letting the first-registered gaggle "win."

### 4. One real budget/backpressure primitive (needs upstream)

A workflow author declaring "bound how many times this remediation loop may
retry, per cause" should set one field, once, and have it apply to every
cause the runtime currently knows about *and every cause it grows later* —
not hand-list N inputs by name and silently 500 the day a workflow forgets
one or the runtime adds an N+1th cause nobody's workflow declared a budget
for yet.

### 5. Reference-repo access decoupled from workspace type (needs upstream)

`contents:read` + a gaggle's `additionalRepos` should give a stage read
access to that reference repo regardless of whether the stage also wants its
own project checked out. "Do I need my own project's working copy" and "do I
need to read a reference repo" are two independent questions; today the
runtime only answers the second one correctly if you also say yes to the
first, for reasons that have nothing to do with what the stage is trying to
do.

### 6. A real run-identity primitive, for every stage type (needs upstream)

Any stage — deterministic or agentic — that wants to say "this is what
produced me" should be able to read its own run/workflow/task identity
without guessing. Provenance shouldn't degrade to "omit it if you can't
figure it out."

### 7. `readiness` caps that mean what they say (today, once the runtime bug's fixed — filed upstream)

`maxOpenPRs`, and any similar per-workflow/per-gaggle ceiling, should be
enforced for every gaggle that sets it, not just whichever one happens to
own the first repo in the instance's repo list. This one's already filed
upstream (the fix is a runtime change, not new DSL surface) — listed here
because the *promise* the field makes is exactly the kind of "config surface
tells the truth" property this vision is about.

### 8. Comment discipline (today — this is on us, not Goobers)

Nothing here needs a new DSL feature. It just needs us to actually do it:
write down what a stage means, not the archaeology of how we found out what
it needed to mean.

### 9. Workflow "lane" variants — nice to have, not a priority (operator call)

`implementation`/`implementation-critical` and `merge-review`/
`merge-review-critical` are each two full, hand-forked workflow files,
differing only in a handful of `readiness`/label-filter/`respectAssignee`
values, kept in sync by hand. In principle this is the same
shared-role-with-parameters problem as item 2, one level up (a "lane" as a
workflow-level variant instead of a goober-level one) — but explicitly
**not** prioritized: copying two workflow files that stay this small and
this rarely-diverging is a reasonable tradeoff against inventing new
workflow-variant DSL surface for it. Left as-is deliberately; revisit only
if the copy actually drifts painfully in practice, not preemptively.

## What's in this pass

**`goobers-site`** — `upstream-sync.yaml` and its four goobers
(`upstream-churn-reporter`, `change-area-researcher`,
`site-relevance-triage`, `site-sync-clerk`): rewritten in full, comments
trimmed to intent, workarounds marked inline with `WISH:` pointers into this
document.

**`goobers`** (the self-hosting gaggle) — the same treatment, extended
across the whole gaggle:

- `quality-sprint.yaml`: its eight dedicated researcher goobers
  (`test-coverage-researcher`, `cross-platform-researcher`,
  `code-quality-researcher`, `docs-researcher`, `reliability-researcher`,
  `performance-researcher`, `ux-researcher`, `latent-bugs-researcher`)
  collapsed into one shared `quality-lens-researcher`, invoked eight times
  with a different `areaFocus` each — item 2's within-gaggle case, applied.
  Workflow comments trimmed to intent.
- `backlog-clerk`: comments and structure cleaned — the earned defensive
  rules (malformation backstop, dedup discipline, trust rubric) are all
  still here, stated directly rather than justified by the incident that
  taught them.
- `implementation.yaml`, `implementation-critical.yaml`,
  `backlog-curation.yaml`, `test-instability-nomination.yaml`: comment
  discipline pass — `SYNCED <date> from main <hash>` changelog blocks and
  bare issue-number citations removed, the actual design rationale kept and
  restated as a direct explanation of the current shape. `implementation`/
  `implementation-critical` are still two files (item 9 — deliberately not
  collapsed).
- `curator`, `implementer`, `reviewer`, `instability-shaper`,
  `quality-triage`, `churn-reporter`: light touch — these were already
  close to intent-first; only stray issue-number citations trimmed.

## What's not — a real second pass, not done here

`merge-review.yaml`, `merge-review-critical.yaml`, and `pr-remediation.yaml`
specifically. These three carry the deepest production history in the
instance — the PRR-1 through PRR-10 remediation-cause design arc, the
merge-queue/election/scope-gate machinery, the #1860 merged-while-running
race hardening. Rewriting these well means understanding *why* each guard
stage and gate exists well enough to state the constraint without the
incident report, which takes reading that history closely rather than
inferring it from the file it's currently embedded in — worth doing as its
own deliberate pass, with the same care this pass gave everything else, not
rushed alongside it.
