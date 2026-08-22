# Gaggle memory — config templates

Copyable templates for adding cross-run memory to a gaggle. Everything here is
generic: replace `<your-gaggle>` with your gaggle name and `model: <fill-in>`
with a model your harness accepts. See `docs/design/agent-memory.md` for the
architecture and `docs/guides/memory.md` for the operator guide.

## Layout

```
config-examples/memory/
  README.md                        this file
  store/                           an example memory store
    MEMORY.md                      generated index
    active/                        one example memory per type
  workflows/
    dream.yaml                     nightly audit -> sync -> consolidate -> promote
    recall-snippet.yaml            recall task + downstream contextFrom wiring
    reflect-snippet.yaml           reflect task (+ parked-run variant)
    wizard-gate-snippet.yaml       wizard-review gate before other reviews
  goobers/
    scribe/                        cheap recall + reflect goober
    wizard/                        memory-informed review gate goober
    wizard-dreamer/                the nightly consolidator goober
```

## How the pieces fit

- **scribe** runs `gaggle-memory recall` on the way in and `gaggle-memory propose`
  on the way out. Cheap and fast; it reads and nominates, never promotes.
- **wizard** is a review gate: "have we been here before?" judged only against
  active memory. Its full prompt is in `goobers/wizard/instructions.md` because
  gate evaluators take no `goal`/`contextFrom`.
- **wizard-dreamer** runs nightly, reads `proposed/` + `active/`, and writes a
  decisions file to `dream/`. It writes only the decisions file; the
  deterministic `promote` stage applies it under the hard rules.

## Adapting the templates

1. Set `spec.gaggle` on each goober and workflow to your gaggle.
2. Set each goober's `model` to something your harness supports (the scribe
   should be cheap/fast; the wizard-dreamer should be your strongest model).
3. Point every `gaggle-memory` invocation's `--store` at your real store path.
4. Add `recall` to your implement task's `contextFrom`, place `wizard-review`
   ahead of your existing review gates, and register the schedule for
   `dream.yaml` when you are ready to run consolidation for real.

The `store/` directory is an example you can copy as a starting point, or
regenerate from a Claude project with `gaggle-memory init-from-claude`.
