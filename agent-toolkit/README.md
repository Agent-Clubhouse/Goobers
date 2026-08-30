# Goobers agent toolkit

This release-owned bundle gives an external coding agent enough Goobers context
to author workflow configuration, inspect local operations, and plan upgrades
without a Goobers source checkout. It is distinct from `Goober.Spec.skills`,
which configures agents invoked inside a Goobers workflow.

## Archive layout

```text
manifest.json
payload/
  .goobers/
    agent-toolkit/
      README.md
      release.json
      instructions/goobers.md
      adapters/
        copilot.md
        claude.md
        agents.md
      skills/
        goobers-getting-started/
        goobers-environment-resolver/
        goobers-dsl-author/
        goobers-run-operator/
        goobers-workflow-upgrade/
      api/schemas/
      config-examples/
      docs/
      internal/capability/
```

`manifest.json` is transport metadata. Its schema is bundled at
`payload/.goobers/agent-toolkit/api/schemas/agent-toolkit-manifest.schema.json`.
The manifest records its own schema and bundle versions, the producing Goobers
release and commit, the release's DSL support matrix, compatible harness
adapters, required and optional CLI commands, and every product-owned payload
file with its size, required mode, and SHA-256 digest.

`goobers agent-kit install` copies the payload into a selected configuration
repository and retains that metadata as
`.goobers/agent-toolkit/manifest.json`. `goobers agent-kit check` uses the
checked-in manifest to report drift, and `goobers agent-kit update` shows a diff
by default. An update writes only with `--write`; replacing a locally modified
owned file additionally requires `--replace-modified`.

Required commands provide release resolution, validation, and the core
read-only operator path. Optional commands, when exposed by the matching
binary, improve diagnostics or automate an upgrade but are not needed to
consume the bundled contracts.

`release.json` carries the release identity and DSL support matrix into an
installed payload. The docs, schemas, examples, capability registry, and skills
are copied from the same source revision as the producing binary.

## Ownership and updates

Only paths listed in `manifest.json` are product-owned assets. They all live
beneath `payload/.goobers/agent-toolkit/`, the bundle's overwrite boundary.
Copy the contents of `payload/` into a config repository to place that
versioned boundary at `.goobers/agent-toolkit/`. The CLI-managed installed
manifest is ownership metadata, not permission to replace unlisted files below
that boundary.

Repository-root `AGENTS.md`, `CLAUDE.md`, and
`.github/copilot-instructions.md` remain user-owned and are never payload
assets. The files under `adapters/` are small templates that a user may include
or merge into those harness instruction files. Replacing a toolkit version may
replace only the product-owned boundary; it must not replace user-owned
instructions. The bundle performs no synchronization or automatic mutation.

## Using the bundle

Choose the adapter for the repository's harness and incorporate its text into
the user-owned instruction file. Start a new configuration with
`goobers-getting-started`; it delegates release resolution and configuration
authoring to the canonical specialist skills. Every adapter points to the same
root instruction and canonical Agent Skills bodies; skill content is not
duplicated per harness.

The environment resolver selects release-matched sources. When no source
checkout is present, it uses the docs, schemas, and examples in this payload.
Links to the project's default branch may provide supplementary context but
never override the producing release's bundled material.
