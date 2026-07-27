# Use the Goobers agent toolkit

The portable [Goobers agent toolkit](../../agent-toolkit/README.md) gives an
external coding agent release-matched skills for environment resolution, DSL
authoring, read-only run inspection, and workflow upgrades. It runs in the
user's agent harness and is distinct from skills configured on workflow
goobers.

## Install the release bundle

From a release-matched `goobers` binary, install the toolkit and the selected
harness reference into a checked-in config source repository non-interactively:

```sh
goobers onboarding stub-agent-instructions \
  --source-tree ./my-config \
  --harness copilot \
  --json
```

The versioned JSON envelope reports the `.goobers/agent-toolkit` root and
instruction file under `created` or `skipped`, the absolute config source path,
and the `agent-kit check` command to run next. Re-running is a no-op. The action
delegates to `goobers agent-kit install`, so it preserves existing instructions,
refuses user-owned toolkit collisions or drift, and never replaces toolkit
content implicitly. The config source path must be a Git repository root.

To install without the onboarding envelope, run the equivalent command:

```sh
goobers agent-kit install --harness copilot ./my-config
```

### Manual archive installation

Download `goobers-agent-toolkit_<version>.zip` from the same release as the
installed Goobers binary and verify it with that release's `SHA256SUMS`. The
archive has a machine-readable `manifest.json` and a copy-ready payload:

```text
payload/
  .goobers/
    agent-toolkit/
```

Copy the contents of `payload/` into the config repository. This is a one-way
copy; there is no watcher or automatic update. The manifest inventories every
product-owned payload file and digest. Product-owned files stay beneath
`.goobers/agent-toolkit/`; repository-root `AGENTS.md`, `CLAUDE.md`, and
`.github/copilot-instructions.md` remain user-owned.

Choose `.goobers/agent-toolkit/adapters/copilot.md`,
`adapters/claude.md`, or `adapters/agents.md` and merge that short adapter into
the harness's user-owned instruction file. Every adapter resolves the same
canonical Agent Skills bodies; it does not duplicate them. The installed
`release.json`, bundled docs, JSON Schemas, examples, and `goobers validate`
from the matching release are authoritative. A source checkout is optional.

## Author workflow DSL

The canonical
[`goobers-dsl-author` skill](../../skills/goobers-dsl-author/SKILL.md) turns a
plain-English process into Gaggle, Goober, and Workflow definitions, explains
the platform's core terms, and points the authoring agent at the matching docs
and schemas. It does not require the Goobers daemon.

## Ask in plain English

Include the target repo, desired cadence, work, decisions, and allowed side
effects when known. Missing optional details receive conservative defaults.
For example:

> Create an `acme-api` gaggle for the GitHub repo and backlog `acme/api` on
> `main`. Every weekday at 09:00, run `go test ./...`. If tests pass, ask a
> triager goober to inspect the repo and file evidence-backed GitHub issues;
> otherwise abort. The triager may read code and write issues but must never
> push code or open pull requests.

The skill first explains the terms involved, chooses an output layout, then
sketches the state graph and generates the applicable paths. For a new
checked-in config source tree, it emits a complete tree that can be validated
directly:

```text
instance.yaml.example
manifest.yaml
gaggles/acme-api/gaggle.yaml
gaggles/acme-api/goobers/triager/goober.yaml
gaggles/acme-api/goobers/triager/instructions.md
gaggles/acme-api/workflows/test-and-triage.yaml
```

The manifest declares the named GitHub repo and backlog connections referenced
by the gaggle. The instance template separately declares the `acme/api` target
repo and environment-variable references for the repo and model credentials,
never secret values; instance files do not contain named connections. For an
existing initialized instance, the skill instead keeps its root-level
`instance.yaml` and writes `manifest.yaml` and `gaggles/` beneath `config/`.

For that request, the graph is a scheduled workflow with a deterministic test
task, an automated status gate, and an agentic triage task. The goober and its
agentic task receive only `agent:model`, `repo:read`, and
`github:issues:write`.

You can also use the skill as a docs finder:

> Use `goobers-dsl-author` to explain the difference between a task and a gate,
> list the valid capability names, and point me to their source of truth.

Its bundled [terminology](../../skills/goobers-dsl-author/references/terminology.md)
and [DSL reference](../../skills/goobers-dsl-author/references/dsl-reference.md)
link to release-matched copies of:

- `docs/requirements/*.md` for semantics;
- `docs/stage-contract.md` for stage data and completion;
- `api/schemas/*.schema.json` for accepted resource and envelope shapes;
- `internal/capability/capability.go` for capability names;
- `config-examples/` for complete patterns.

## Validate the generated config

Validation is local and does not require `goobers up`:

```sh
# An initialized instance containing instance.yaml and config/
goobers validate ./my-instance

# A checked-in config source tree containing instance.yaml.example,
# manifest.yaml, and gaggles/
goobers validate --source-tree ./my-config
```

The `--source-tree` path is the config root itself: do not add an extra
`config/` directory beneath it. The initialized-instance command instead
expects `instance.yaml` and `config/` beneath the supplied path.

The validator checks more than JSON Schema: it compiles workflows to catch
broken state references, unreachable states, invalid schedules, incomplete
gate outcomes, unknown capabilities, and task grants that exceed a goober's
grants. Give validation errors back to the same authoring agent and have it
repair the generated files before committing them.

If an initialized instance already exists, the agent may use
`goobers scaffold goober` and `goobers scaffold workflow` as validated
baselines, then replace the generic goals, states, grants, and instructions
with the requested process.
