---
name: goobers-workflow-upgrade
description: Compare a Goobers workflow with a target release and plan or apply an explicit, reviewable DSL upgrade using release-matched schemas and validation.
---

# Goobers workflow upgrade

Upgrade configuration deliberately; never reinterpret it against the newest
default branch or silently bump its DSL version. Resolve the current toolkit,
binary, and source set with `goobers-environment-resolver` first.

## Upgrade procedure

1. Read each workflow's explicit `dslVersion` and run `goobers versions --json`
   for the target binary. Select a target version whose lifecycle level permits
   loading. Treat preview versions as opt-in and unsupported versions as load
   errors.
2. Compare the current and target contracts using this toolkit's
   release-matched `docs/`, `api/schemas/`, `config-examples/`, and
   `release.json`. Use `goobers features --dsl-version <version>` when that
   optional command is available.
3. Produce a reviewable migration plan before editing. Identify field changes,
   state-graph changes, stage input/output vocabulary, capabilities, and
   behavior changes separately.
4. If the target binary exposes a `goobers fix --to <version>` command, use it
   only as an author-requested starting point and review its diff. Otherwise
   make the smallest manual change supported by the bundled target schemas.
   Never assume a migration command exists.
5. Update one workflow version boundary at a time. Preserve user-owned
   instructions and unrelated definitions.
6. Run `goobers validate` against the target release and repair schema,
   reference, reachability, gate-outcome, and capability-admission errors.
   Report any remaining semantic review that validation cannot prove.

Do not start workflows or mutate runtime state as part of an upgrade. Links to
the default branch may explain later changes but are not authoritative for the
target release.
