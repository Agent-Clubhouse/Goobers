# Run multiple Goobers instances against one repo

This guide is for the case where several independently-run Goobers instances —
different teams, different machines, no shared infrastructure between them —
each want to work a **region** of the same target repository. It documents the
recommended Phase 1 pattern: local, no shared coordination substrate, primarily
prevent overlap by construction rather than lean on a lock that does not exist
yet.

It resolves the documentation gap tracked in
[#1657](https://github.com/Agent-Clubhouse/Goobers/issues/1657) and is MIRC-1
of [the multi-instance coordination design](../design/v1/multi-instance-repo-coordination.md)
([#1899](https://github.com/Agent-Clubhouse/Goobers/issues/1899)).

## The motivating case

A many-million-line brownfield monorepo. 10-20 teams each want to run their own
Goobers instance, from their own machine, against their own region of the repo
(`/frontend`, `/services/billing`, `/infra`, ...) — before anyone commits to a
shared, centrally-hosted instance. This is a real, common shape: independent
teams fit-assessing Goobers locally, each pointed at the same `Project.Owner`
and `Project.Name`, each with their own gaggle config.

Nothing stops two of those instances from claiming the same backlog item or
opening competing PRs unless their configs are set up to avoid it.

## The recommended pattern: pair a required label with a distinct branch namespace

Two independent, already-shipped mechanisms, used **together**:

1. **A disjoint required label per team**, via `backlog-query`'s existing
   `requireLabels` input. Each team's `implementation`/`backlog-curation`
   workflows only claim items carrying their own team's label — nothing
   provider-specific, GitHub and ADO both support required-label filtering the
   same way.
2. **A distinct `BranchNamespace` per gaggle** ([#965](https://github.com/Agent-Clubhouse/Goobers/issues/965)/[#1010](https://github.com/Agent-Clubhouse/Goobers/issues/1010)/[#1109](https://github.com/Agent-Clubhouse/Goobers/issues/1109), already shipped). This is a
   **structural** partition on the PR-lifecycle side: `pr-select`,
   `gather-sibling-context`, `apply-verdict`, `elect-lander`, merge/post-merge,
   `issue-close-out`, and the mirror-fetch prune exclusion all filter by the
   gaggle's configured branch prefix. Two gaggles with distinct namespaces
   structurally never see, review, or merge each other's PRs.

Use both. The required label scopes **which backlog items** an instance will
claim; the branch namespace scopes **which PRs** an instance will act on. They
are independent knobs solving different halves of the same problem, and
skipping either one leaves a real gap: label-only partitioning still lets one
instance's `merge-pr`/`pr-select` stages see and act on another team's PRs;
namespace-only partitioning still lets two instances claim the same issue.

### Opting merge-review into an instance

Existing merge-review workflows retain the historical branch-prefix and
starvation behavior. To prevent independently configured instances from
observing the same PR, add these inputs to `pr-select`:

```yaml
requireOptInLabel: "goobers:team-frontend"
respectAssignee: "true"
selfIdentity: "frontend-goobers"
```

These rules compose: the label must be present, and the configured identity
must be an assignee or requested reviewer. Starvation protection ranks only
the resulting eligible set, so it cannot bypass either restriction. Thread
`publishAdvisory: "false"` into `apply-verdict` when outside-prefix advisory
reviews should remain read-only. Leave the inputs empty or false during
migration to preserve legacy behavior.

## Worked example: two teams, one repo

Team **frontend** and team **billing** both run their own instance against the
same repo. The repo's backlog items are labeled by area (`area:frontend`,
`area:billing`) as part of normal triage.

Frontend's gaggle config:

```yaml
# gaggles/frontend/gaggle.yaml
apiVersion: goobers.dev/v1alpha1
kind: Gaggle
metadata:
  name: frontend
spec:
  displayName: Frontend
  project:
    provider: github
    owner: acme
    name: monorepo
    branch: main
  backlog:
    provider: github
    project: acme/monorepo
    labels:
      - goobers
  # Structural PR-lifecycle partition: frontend's runs only ever push, select,
  # review, and merge branches under this prefix.
  branchNamespace: "goobers-frontend/"
```

```yaml
# gaggles/frontend/workflows/implementation.yaml (excerpt)
- id: claim-work
  goal: Claim one ready, trust-approved issue to implement.
  run:
    command: ["goobers", "backlog-query", "--claim"]
  inputs:
    trustLabel: "goobers:approved"
    requireLabels: "goobers:ready,area:frontend"
```

> **`connectionRef` is not a runtime credential selector.** The local runner
> resolves every access's token from `instance.yaml` `repos[]` by repository
> identity, never from the named Connection, so declaring one connection for
> the project and another for the backlog would not route the two accesses
> through two credentials. `goobers validate` reports `REF012` (#3296)
> wherever the field is declared, and the shipped configs leave it out
> for that reason. Scope the token itself in `instance.yaml` when a narrower
> one is wanted.

Billing's gaggle config is the mirror image — same `project.owner`/`project.name`
(same repo), different `branchNamespace`, different `requireLabels`:

```yaml
# gaggles/billing/gaggle.yaml
spec:
  displayName: Billing
  project:
    provider: github
    owner: acme
    name: monorepo
    branch: main
  branchNamespace: "goobers-billing/"
```

```yaml
# gaggles/billing/workflows/implementation.yaml (excerpt)
inputs:
  trustLabel: "goobers:approved"
  requireLabels: "goobers:ready,area:billing"
```

With this pairing in place: frontend's `backlog-query` never becomes eligible
for a `area:billing`-only item (and vice versa), and frontend's PR-lifecycle
stages never enumerate, review, or merge a `goobers-billing/*` branch. The two
instances behave as if they were working separate repos, without either one
needing to know the other exists.

## This is soft partitioning, not a hard guarantee

Say this plainly rather than overselling it: **the required-label pairing has
a residual race window.** The claim ledger
(`internal/localscheduler/claim.go`) is per-instance-local; the only
cross-instance signal is the provider-visible `goobers:claimed` label/assignee
mirror, and that mirror is documented as non-authoritative — a reflection for
human visibility, not claim truth. GitHub/ADO label and assignee mutation is
not an atomic compare-and-set. Two instances can both observe an item
unclaimed, both write the claim marker, and both proceed. This is a genuine
race, not a hypothetical one.

In practice, teams whose regions are genuinely disjoint by label essentially
never hit this — the race only matters when scopes overlap or a config is
misconfigured to overlap unintentionally. Two hardening layers build on top of
this guide once they ship, and are worth adopting once available:

- **Sibling-scope declaration + overlap validation** ([MIRC-2, #1901](https://github.com/Agent-Clubhouse/Goobers/issues/1901)) —
  `goobers validate`/`lint` will warn when two gaggles that declare each other
  as siblings (matched by target repo, not by gaggle name) have required-label
  sets that are not actually disjoint, catching a misconfiguration before it
  produces a live collision.
- **Claim-race detection** ([MIRC-3, #1902](https://github.com/Agent-Clubhouse/Goobers/issues/1902)) — a write-then-reread hardening in
  the claim path that lets the losing instance detect it lost a race and back
  off, instead of silently duplicating work.

Neither of those makes the underlying operation atomic — that guarantee only
exists once instances share a real coordination substrate (Phase 2: a shared
Temporal deployment, or an equivalent shared claim store). For the local,
independently-run phase this guide covers, correct-by-construction label and
namespace partitioning is the primary defense; the hardening above is defense
in depth on top of it, not a substitute for it.

## Complementary option: assignee-based partitioning (COORD)

Required labels partition by **area**. If a region has enough churn that
label-only partitioning is not granular enough — e.g. within one team's area,
individual members want issues routed to themselves specifically — COORD's
opt-in assignee-based dispatch (`respectAssignee`/`assignedTo` inputs on
`backlog-query`, [#1820](https://github.com/Agent-Clubhouse/Goobers/issues/1820)) is a second, independent partitioning axis that
composes with the label pattern above rather than replacing it. It is also
usable as a finer-grained cross-instance claim-scoping mechanism in its own
right, not just for human/bot task division. See
[the COORD design](../design/v1/backlog-assignment-coordination.md) for the
full shape.

## See also

- [Design: Multiple Goober instances coordinating on the same repo](../design/v1/multi-instance-repo-coordination.md) —
  the full design this guide implements MIRC-1 of.
- [#1657](https://github.com/Agent-Clubhouse/Goobers/issues/1657) — the original
  documentation ask this guide resolves.
- [Design: Assignment-aware backlog coordination (COORD)](../design/v1/backlog-assignment-coordination.md) —
  the complementary assignee-based partitioning axis.
