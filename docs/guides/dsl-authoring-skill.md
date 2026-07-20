# Author Goobers DSL with your own agent

The portable [`goobers-dsl-author` skill](../../skills/goobers-dsl-author/SKILL.md)
turns a plain-English process into Gaggle, Goober, and Workflow definitions,
explains the platform's core terms, and points the authoring agent at the
canonical docs and schemas. It runs in the user's agent harness; it does not
invoke or require the Goobers daemon.

## Install the skill

The package follows the Agent Skills directory convention: one `SKILL.md` plus
supporting files under `references/`. Copy the entire directory into the skill
location used by your harness:

```sh
cp -R skills/goobers-dsl-author <your-agent-skills-directory>/
```

Register or enable `goobers-dsl-author` using that harness's normal skill
discovery mechanism. If the harness does not discover `SKILL.md` packages,
attach `SKILL.md` and both reference files to a custom agent's instructions.
Keep the package intact so its relative reference links continue to work.

Use the skill version from the same Goobers release as the config you are
authoring. The bundled quick reference is portable, while that release's JSON
Schemas and `goobers validate` remain authoritative.

## Ask in plain English

Include the target repo, desired cadence, work, decisions, and allowed side
effects when known. Missing optional details receive conservative defaults.
For example:

> Create an `acme-api` gaggle for the GitHub repo and backlog `acme/api` on
> `main`. Every weekday at 09:00, run `go test ./...`. If tests pass, ask a
> triager goober to inspect the repo and file evidence-backed GitHub issues;
> otherwise abort. The triager may read code and write issues but must never
> push code or open pull requests.

The skill first explains the terms involved, then sketches the state graph and
generates the applicable paths:

```text
config/manifest.yaml
config/gaggles/acme-api/gaggle.yaml
config/gaggles/acme-api/goobers/triager/goober.yaml
config/gaggles/acme-api/goobers/triager/instructions.md
config/gaggles/acme-api/workflows/test-and-triage.yaml
```

For that request, the graph is a scheduled workflow with a deterministic test
task, an automated status gate, and an agentic triage task. The goober and its
agentic task receive only `agent:model`, `repo:read`, and
`github:issues:write`.

You can also use the skill as a docs finder:

> Use `goobers-dsl-author` to explain the difference between a task and a gate,
> list the valid capability names, and point me to their source of truth.

Its bundled [terminology](../../skills/goobers-dsl-author/references/terminology.md)
and [DSL reference](../../skills/goobers-dsl-author/references/dsl-reference.md)
link to:

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

The validator checks more than JSON Schema: it compiles workflows to catch
broken state references, unreachable states, invalid schedules, incomplete
gate outcomes, unknown capabilities, and task grants that exceed a goober's
grants. Give validation errors back to the same authoring agent and have it
repair the generated files before committing them.

If an initialized instance already exists, the agent may use
`goobers scaffold goober` and `goobers scaffold workflow` as validated
baselines, then replace the generic goals, states, grants, and instructions
with the requested process.
