---
name: goobers-dsl-author
description: Explain the Goobers DSL, find its canonical documentation, and turn a plain-English workforce or workflow description into schema-valid Gaggle, Goober, and Workflow YAML.
---

# Goobers DSL author

Translate a user's intent into the smallest complete Goobers config change. This
skill authors definitions for the user's config repo; it does not start or
depend on the Goobers daemon.

## Find the canonical sources

When a Goobers source checkout is available, inspect the matching release's
sources before authoring:

1. `docs/ARCHITECTURE.md` for architecture and terminology precedence.
2. `api/schemas/gaggle.schema.json`, `goober.schema.json`, and
   `workflow.schema.json` for the accepted YAML shape.
3. `docs/requirements/{gaggle,goober,workflow,task,gate,config-as-code}.md`
   for semantics.
4. `docs/stage-contract.md` for task inputs, outputs, artifacts, and
   capabilities.
5. `internal/capability/capability.go` for the canonical capability registry.
6. `config-examples/` for complete patterns. Examples illustrate the schemas;
   they do not override them.

Outside a checkout, use the bundled
[`references/dsl-reference.md`](references/dsl-reference.md) and
[`references/terminology.md`](references/terminology.md). If the user's
Goobers version is known, consult sources from that release rather than `main`.

If sources disagree, follow `docs/ARCHITECTURE.md`, then the schemas and
validator for the target release. Do not invent fields from prose or examples.

## Authoring procedure

1. **Explain the nouns.** Give the user the short definitions of gaggle,
   goober, workflow, task, gate, trigger, and capability from
   `references/terminology.md`. Keep this to the terms relevant to the request
   unless the user asks for the full glossary.
2. **Extract a model from the description.** Identify:
   - target project and singleton backlog;
   - trigger and readiness limits;
   - ordered task states, including which are deterministic or agentic;
   - branch decisions and their target states;
   - one goober role per distinct agentic responsibility;
   - the least-privilege capabilities each task actually needs.
3. **State assumptions instead of blocking on optional details.** Prefer a
   `manual` trigger, `main` branch, `maxConcurrentRuns: 1`, GitHub provider,
   and no write capabilities when the description does not require another
   choice. Use lowercase DNS-style slugs derived from the user's names. Never
   guess credentials or place a secret in YAML.
4. **Choose the target layout.** Do this before assigning paths:
   - For a checked-in config source tree, put `instance.yaml.example`,
     `manifest.yaml`, and `gaggles/` at the repository root. This is the
     default when the user asks for a new config repo.
   - For an initialized instance, use its existing `instance.yaml` and put
     definitions under `config/`. Update `instance.yaml` only when the request
     changes instance-level connections or settings.

   These layouts are not interchangeable. Populate the instance file or
   template with the requested target repo and provider, plus the credential
   references required by the generated capabilities. Use only env or file
   references, never secret values.
5. **Sketch the state graph.** Show `start -> task -> gate(outcome) -> target`
   before writing YAML. Every referenced state must exist and every declared
   state must be reachable.
6. **Generate the files.** For a new gaggle in a checked-in config source tree,
   normally produce:

   ```text
   instance.yaml.example                   # valid, secret-free instance template
   manifest.yaml                           # add the gaggle name when applicable
   gaggles/<gaggle>/
     gaggle.yaml
     goobers/<goober>/
       goober.yaml
       instructions.md
     workflows/<workflow>.yaml
   ```

   In an initialized instance, use the same definition tree beneath `config/`
   and retain its root-level `instance.yaml`:

   ```text
   instance.yaml
   config/
     manifest.yaml
     gaggles/<gaggle>/
       gaggle.yaml
       goobers/<goober>/
         goober.yaml
         instructions.md
       workflows/<workflow>.yaml
   ```

   If the user already has a gaggle, only create or update the definitions
   required by the request. A goober needs matching `instructions.md`; do not
   emit a YAML-only worker that references a missing file.
7. **Validate and repair.** Run the target release's validator when available,
   fix every error, and rerun it. Schema checks alone are insufficient because
   state reachability, cross-references, schedules, gate outcomes, and
   capability admission are compiler checks.

## Generation rules

Apply the complete checklist in `references/dsl-reference.md`. In particular:

- Use exactly `apiVersion: goobers.dev/v1alpha1` and the appropriate `kind`.
- Keep undocumented keys out; the definition schemas are closed.
- Quote cron expressions, selector values, and values under `inputs` or
  `inputsFrom`.
- Give each workflow at least one trigger and a readiness limit. A `manual`
  trigger must be the only trigger.
- An agentic task has `goober` and no `run`; a deterministic task has `run`
  and no `goober`.
- Put only canonical capabilities on tasks. Every capability on an agentic
  task must also be granted by its goober. Include `agent:model` on a
  Copilot-backed goober and on tasks that invoke it.
- Give a gate exactly one evaluator block. Define every outcome the evaluator
  can return, including `pass`, `fail`, and `needs-changes` for an agentic
  gate.
- Omit `next` for successful completion. Use `@abort` or `@escalate` only for
  explicit non-success terminals.
- Pass non-scalar stage data through content-digested artifact pointers.
  Result `outputs` are scalars only.
- Reference secrets and connections by name; never emit tokens, passwords, or
  credential values.

For an initialized, single-gaggle instance, `goobers scaffold goober <name>
<instance>` and `goobers scaffold workflow <name> <instance>` may provide a
validated baseline. Tailor every generated goal, task, gate, grant, and
instruction to the description; do not return the generic scaffold unchanged.
Do not use `--force` unless replacement was explicitly requested.

## Validation commands

Use the command matching the user's layout:

```sh
# Initialized instance: instance.yaml plus config/
goobers validate ./my-instance

# Checked-in source tree: instance.yaml.example, manifest.yaml, and gaggles/
goobers validate --source-tree ./my-config
```

From a Goobers source checkout without an installed binary:

```sh
go run ./cmd/goobers validate --source-tree /path/to/my-config
```

Validation does not require `goobers up` or any running service. Do not claim
the result is validated if no validator or equivalent schema/compiler check
was run; identify the remaining check instead.

## Deliver the result

Return or write:

1. a concise term explanation;
2. assumptions and the state graph;
3. each created or changed file at its intended config-repo path;
4. the capabilities selected and why;
5. the validation command and actionable errors, if any.

Do not mix explanatory prose into a YAML code block. Keep generated YAML ready
to copy directly into the config repo.
