# Tutor config-only write-boundary

The Tutor is a self-tuning loop: it mines a Goobers instance's own telemetry for
recurring problems and opens a **config-as-code pull request** proposing a fix
(a gaggle workflow or a goober's instructions). Because that PR is authored by
the system about the system, it must be **structurally confined to config** — a
Tutor run can never touch platform code, CI, credentials, or anything outside the
instance's config.

This guide covers the **path-scoped** half of that boundary (issue #104 / design
§T4, wired into the real Tutor architecture by #223). It ships today. The
**structural** half — a credential that cannot push platform changes even if this
check were bypassed — is deferred to #35 (see
[What #35 adds](#what-35-adds-structural-enforcement)).

## What the boundary does

The Tutor workflow (`tutor.yaml`) drafts its config change in the run's git
worktree (the `draft-change` stage commits + pushes to the run branch), then the
`open-pr` stage opens the PR. Before `open-pr` opens anything, it lists every
file the run's branch changes — `git diff --name-only <base>...HEAD` in that
worktree — and checks each is contained within the instance's **configured config
root** (`internal/configboundary.Confine`). If any changed file falls outside the
root, the stage **fails closed** (`ErrOutsideConfigRoot`): it exits non-zero and
**no PR is opened**, so a self-improvement run can never open a PR touching
platform code. An unverifiable diff (e.g. git failure) is treated as a breach and
also refuses the PR.

The check is **opt-in**: `open-pr` only enforces it when the workflow sets the
`confineToConfigRoot=true` stage input (with `configRoot=<root>`). Other workflows
(implementation, work-nomination) leave it off and are unaffected.

The root is the **instance's configured config root — not a hardcoded
`config/`**. On the dogfood instance (`selfhost/`) the root is `selfhost`;
another instance may back its config with an entirely separate repo.

## Configuring the root

The config root is repo-relative and set per instance. Two shapes:

| Instance shape | Config root | Boundary behavior |
| --- | --- | --- |
| **Separate config repo** — config lives in its own repo with no platform code | empty (whole repo is config) | any in-repo path allowed; absolute paths and `..`-escapes still refused |
| **Same repo** — config and platform code share one repo (the dogfood case) | **must be a non-empty subtree**, e.g. `selfhost` | only paths under that subtree are allowed; every platform path is refused |

> **Same-repo instances MUST set a non-empty root.** With an empty root the
> boundary only prevents escaping the repository, not reaching platform paths
> inside it. On the dogfood repo the root is `selfhost`, so `internal/…`,
> `.github/…`, `Makefile`, `providers/…` and every other platform path are
> unreachable through the Tutor.

A root that is itself bogus (absolute, `.`, `..`, or escaping) is treated as
*unset* (the whole-repo floor) rather than trusted — a bad root can never widen
the boundary.

## Governance: CODEOWNERS + branch protection

Path-scoping keeps the Tutor *in* config; it does not decide whether a config
change is *good*. That judgement is a human's, enforced by review:

- **CODEOWNERS on the config root.** `.github/CODEOWNERS` owns `/selfhost/`, so a
  Tutor PR to the config root requests a CODEOWNER and — once branch protection
  requires CODEOWNER review — cannot merge without a maintainer's approval.
- **Branch protection on `main`.** The instance never merges (there is no merge
  stage in any workflow); the required `make ci` check plus a human review are
  the only path to `main`. See `selfhost/README.md`.

## Enablement checklist (before turning the Tutor on for the dogfood repo)

Tutor PRs land in the **same repo as platform code** on the dogfood instance, so
before enabling it there:

1. **Config root is set to a non-empty subtree** (`selfhost`) — never empty.
2. **CODEOWNERS covers that root** (`/selfhost/` → a maintainer/team) — present
   in `.github/CODEOWNERS`.
3. **Branch protection requires CODEOWNER review** on `main` so the ownership is
   load-bearing, not advisory.
4. The Tutor's credential is **read + config-write only** where possible (full
   structural scoping lands with #35).

## What #35 adds (structural enforcement)

Today the boundary is enforced by the `open-pr` stage checking the run's git diff
before it opens the PR. #35 (per-goober credential injection) adds the second
layer: a token scoped so that even a compromised or buggy Tutor **physically
cannot** push a change outside config. Until then, path-scoping + CODEOWNERS +
branch protection are the boundary; keep all three in place.

## Testing the boundary

Two layers of tests cover it. The containment logic
(`internal/configboundary/configboundary_test.go`) is exercised against a
**non-default** root (`selfhost`, plus arbitrary custom roots) and asserts that
every platform path — `internal/…`, `.github/…`, `Makefile`, `../…`, absolute
paths, and even the *default* `config/…` — is refused, proving platform paths are
unreachable and that the check honors the configured root rather than a hardcoded
one. The end-to-end negative test (`cmd/goobers/configboundary_test.go`) drives
the **real `open-pr` stage** over a git worktree whose run branch touches a
platform file, and asserts the stage fails closed and opens **no** PR.
