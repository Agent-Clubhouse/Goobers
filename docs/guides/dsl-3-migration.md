# Move workflows from DSL 2.0 to 3.0

DSL 3.0 makes runner placement and repository handoffs explicit. A local
workflow can still omit `runsOn`, resource minimums, and an instance `runners:`
inventory. Add placement requirements when the workflow needs them; migration
does not invent scheduling tags or require new runner flags.

The version pin belongs on each Workflow. Gaggles are unversioned, but their
shared placement requirements must be upgraded with the workflows that use them.

## Review, migrate, and validate

Start with a committed configuration or a disposable copy. The commands take an
**instance root**, containing `instance.yaml` and `config/`, rather than a single
workflow file or the `config/` directory itself:

```console
goobers validate /path/to/instance
goobers fix --to 3.0 /path/to/instance
```

The second command prints unified diffs and migration notes without writing.
Review the workflow changes and any shared Gaggle changes, then apply and check:

```console
goobers fix --to 3.0 --write /path/to/instance
goobers validate /path/to/instance
```

For a checked-in config source tree, work in a disposable instance with that tree
copied into `config/`. Bind `instance.yaml` to the repositories declared by the
Gaggles. `goobers validate --source-tree` can validate the source tree directly,
but `fix` still expects the instance layout.

`fix` can work on a parseable config that already has validation errors. It
prints those findings before attempting migration; an exit of zero from `fix`
does not replace a successful `validate`. Fix unrelated errors separately.

Migration proceeds one registered version step at a time. A workflow on 1.4 must
first pass through `fix --to 2.0`. Already-at-target workflows are left unchanged.
A workflow already on 3.1 cannot be migrated backward with `--to 3.0`: that file
is reported as a refused hop, and the command returns nonzero even when other
files migrated. Read each `FIX` result and the final diff. File writes are not a
transaction across the whole configuration.

If the target version is preview in your binary, validation reports `DVL011`
unless **each Workflow** acknowledges it:

```yaml
metadata:
  name: implementation
  annotations:
    goobers.dev/allow-preview-features: "true"
```

A Manifest or Gaggle annotation does not authorize the Workflow. A stable version
pin does not need this acknowledgement merely for the pin; features that remain
preview can still require it. This includes preview declarations on an associated
Goober, such as an explicit `mcpServers[].required` value in DSL 3.1. Each associated
Workflow must acknowledge the preview and support the field, even when several
consumers use the same DSL version. One consumer's acknowledgement does not
authorize another, and consent does not make a 3.1-only field valid for an older
consumer. Do not add the deprecated Manifest annotation for these declarations.
The migrator does not silently add consent to preview features. See
[DSL versioning](../design/dsl-version-lifecycle.md) for lifecycle policy.

## What the migration changes

| DSL 2.0 | DSL 3.0 |
| --- | --- |
| Workflow `dslVersion: "2.0"` | `dslVersion: "3.0"` |
| Task `requiredCapabilities` | Task `runsOn.capabilities` |
| Gaggle `spec.requiredCapabilities` | Gaggle `spec.runsOn.capabilities` |
| `os=linux`, `os=windows`, `os=darwin` placement tokens | `runsOn.os: linux`, `windows`, `macOS` |
| Task `run.network: none` | `runsOn.restrictions: [network:none]` |
| Implicit repository workspace reuse | Declared `repoFrom` handoffs |

For example, a task's placement requirement changes from:

```yaml
requiredCapabilities: [os=darwin, xcode]
```

to:

```yaml
runsOn:
  os: macOS
  capabilities: [xcode]
```

Task `capabilities` and Goober capability grants authorize actions; they are
separate from runner placement and are not renamed.

The migrator derives `repoFrom` from the workflow graph, including gate routes
and loops. A single producer becomes a scalar; possible producers from different
paths become a list. Review these edges alongside repository writes and push
stages. The migrator uses the interpreter's own handoff analysis so its output
agrees with validation.

Shared Gaggle requirements are preflighted before workflow changes and written
only after the gaggle's workflows migrate successfully. Gaggles keep their
unversioned schema: no `dslVersion` is added. A rejected shared requirement leaves
that gaggle's workflows unchanged; a rejected workflow leaves the shared floor
unchanged. Resolve the refusal and rerun before starting the instance.

The migrator refuses conflicting or unsupported OS tokens, an invalid graph
whose handoffs cannot be computed, and a legacy Gaggle sandbox override without
an implemented translation. It also refuses aliases, merge keys, duplicate keys,
and multiple YAML documents in a file. Expand ambiguous YAML and resolve the
reported requirement deliberately. When both old and new Gaggle placement fields
exist, combine their meaning explicitly; the migrator will not overwrite one.

## Local execution and distributed placement

With no `runners:` block, the instance uses its implicit self runner. A simple
local task does not need a placement block:

```yaml
dslVersion: "3.0"
# apiVersion, kind, metadata, and other workflow fields omitted here
spec:
  gaggle: example
  triggers: [{type: manual}]
  start: inspect
  tasks:
    - name: inspect
      type: deterministic
      goal: Inspect the working copy.
      run:
        command: [git, status, --short]
```

On an implicit local instance, unsatisfied runner capabilities and OS placement
produce warnings during validation. Resource shortfalls are advisory on local
inventories, including a declared self-only inventory. An agentic gate with
`runsOn` may omit CPU or memory locally and receives an advisory. An agentic gate
without `runsOn` continues to evaluate in the control plane.

An instance declaring distributed runners must provide the capabilities and
resources needed by its stages. A placed agentic gate must declare CPU and memory;
missing quantities or unsatisfiable distributed placement are errors. A declared
self-only inventory also makes unsatisfied capability placement strict. Validation
keeps an undeclared self OS advisory because the validating machine need not be
the execution host.

Warnings do not install toolchains or make an incompatible host able to run a
stage. Runtime admission still checks the actual host and claimed capabilities.
Malformed quantities, invalid restriction names, contradictory declarations,
missing reviewer definitions, and broken workflow graphs remain errors locally.
Isolation requirements are not silently discarded. `--strict` can promote
warnings according to the validator's strict-mode policy.

## Continue to 3.1 when needed

After validating 3.0, review the next hop separately:

```console
goobers fix --to 3.1 /path/to/instance
goobers fix --to 3.1 --write /path/to/instance
goobers validate /path/to/instance
```

This migration changes the version pin and reports stages whose
`expectedOutputs` become enforced runtime contracts: a stage that succeeds
without a declared output key can now fail. Check those producers before running
the workflow. Existing positional artifacts remain supported; the migrator does
not infer named artifact slots or consumer bindings from output names. New 3.1
features are opt-ins. Apply the preview acknowledgement rule above when required
by the binary's lifecycle policy.

DSL 3.1 also makes external MCP startup policy uniform across harnesses.
`mcpServers[].required` is available only at this pin, and omission means
required: a failed registration or startup stops the stage before its model
turn. Set `required: false` for a best-effort server whose failure should be a
journal warning. DSL 2.0 and 3.0 reject the explicit field and preserve their
historical harness-specific omitted-field behavior. Set `dslVersion: "3.1"` to
opt into uniform MCP requiredness; migration does not silently change older
pins.

A Goober that explicitly sets `required` cannot also be consumed by a DSL 2.0
or 3.0 Workflow. When the field is omitted, each consuming Workflow keeps its
version's default: compiling a 3.1 Workflow does not rewrite the shared Goober
declaration or change an older Workflow's MCP behavior.

Claude Code does not emit its `system/init` MCP status until after the prompt
has been submitted. For a required server failure, Goobers aborts as soon as
that init status arrives, before accepting an assistant or tool event; the
prompt may already have been submitted when the abort fires. No separate MCP
server or tool probe is launched.
