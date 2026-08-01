# Config-repo PR validation gate

A config-as-code repo with multiple merge-access contributors needs a way to
reject a broken PR before it ever reaches `main`, since the daemon/operator
follows `main` directly (`docs/design/v2-cloud-scale.md` §6 E1). Goobers does
not reinvent repo authorization for this: **branch protection on the config
repo, with this check marked required, is the authorization mechanism.** This
guide sets up that required check.

## What the check validates

`goobers validate --source-tree` is the same validation engine the daemon
runs on every reconcile (`cmd/goobers/configwarnings.go`'s `loadConfigDirectory`
is the identical function value both call — see `cmd/goobers/configreload.go`
and `cmd/goobers/validate.go`) — a config-repo PR that passes this check
cannot fail the daemon's own apply-time validation later. It includes
feature-compatibility checking: a workflow that uses a DSL feature beyond what
its `dslVersion` supports (`internal/workflow.CheckWorkflowFeatureSupport`) is
flagged the same way here as at daemon startup.

## Add the check to your config repo

```yaml
# .github/workflows/validate.yml, in your config repo
name: Validate Goobers config
on:
  pull_request:
permissions:
  contents: read
jobs:
  validate:
    runs-on: ubuntu-latest
    steps:
      - uses: actions/checkout@v7
      - uses: Agent-Clubhouse/Goobers/.github/actions/validate@v1.0.0
        with:
          version: v1.0.0 # pin explicitly — see "Pinning a version" below
          path: . # the directory containing instance.yaml.example, manifest.yaml, gaggles/
          strict: "true" # optional: also fail on warnings
```

The action downloads and checksum-verifies the pinned Goobers release, runs
`goobers validate --source-tree --github-annotations` against `path`, and
fails the step on any validation error — findings that touch a file also
annotate that file's lines directly on the PR diff.

## Pinning a version

The action's `version` input has no default: an unpinned "latest" would let a
config repo's gate change behavior on an upstream Goobers release the config
repo's owners never reviewed. Pin an exact release tag and bump it
deliberately, the same way you'd bump any other pinned Action.

## Require the check

In the config repo's branch protection settings for `main`:

1. Enable "Require status checks to pass before merging."
2. Add the `validate` job (or your chosen job name) from the workflow above
   to the required list.
3. Optionally require review approval too — the check only proves the config
   is structurally valid, not that the change is a good idea.

With the check required, an invalid config PR cannot merge, and `main` stays
something the daemon can always apply.

## Feature-registry compatibility

The check's DSL-feature-support pass is the same one `goobers validate`
already runs locally (`internal/workflow.CheckWorkflowFeatureSupport`/
`CheckGooberFeatureSupport`, gated on each workflow's `dslVersion`) — it is
not the separate, not-yet-built cross-instance version registry described in
`docs/design/dsl-version-lifecycle.md` (DVL-3). A config repo gets real
feature-compatibility enforcement today; the cross-instance registry is a
future enrichment of the same signal, not a prerequisite for it.

## Self-test fixtures

This repo exercises the underlying `goobers validate --source-tree
--github-annotations` command against two checked-in fixtures
(`.github/workflows/config-validate-gate-selftest.yml`):

- `test/fixtures/validate-gate/valid` — expected to pass.
- `test/fixtures/validate-gate/invalid` — a workflow that names a
  non-existent gaggle, expected to fail closed.

`test/fixtures/validate-gate/valid` is also part of `make ci`'s standing
config-fixture sweep (`test/configvalidate`), so it cannot silently drift out
of sync with the validator it exists to demonstrate.
