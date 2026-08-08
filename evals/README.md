# evals/

Design and implementation home for EvalSuite (deterministic, reproducible
evaluation of agentic workflows — [#2662](https://github.com/Agent-Clubhouse/Goobers/issues/2662)).

## Design docs

- [`EVALS_SANDBOX_API.md`](./EVALS_SANDBOX_API.md) — the tool-adapter API
  contract (`real` / `mock` / `replay` / `no-op` modes) and security
  guidelines for shadow/dark runs.
- [`EVALS_CASSETTE.md`](./EVALS_CASSETTE.md) — the cassette storage format
  that `replay` mode reads from and `real`+`record` sessions write to.

Other design docs for sibling child issues under the epic (DSL/schema, judge
harness, adapter shim prototype, runner integration, CI gating) land here as
their issues are worked.
