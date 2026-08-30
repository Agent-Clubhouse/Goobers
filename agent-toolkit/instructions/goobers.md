# Goobers repository assistance

Use the release-matched skills in `.goobers/agent-toolkit/skills/` when working
with this repository's Goobers configuration:

- use `goobers-getting-started` as the default entry point for a new target
  repository or configuration source;
- start other tasks with `goobers-environment-resolver` to establish the
  installed release and authoritative source set;
- use `goobers-dsl-author` to explain or author Gaggle, Goober, and Workflow
  definitions;
- use `goobers-run-operator` for read-only inspection of instance and run state;
- use `goobers-workflow-upgrade` to assess and plan an explicit DSL upgrade.

Prefer `.goobers/agent-toolkit/release.json` and the bundled docs, schemas, and
examples over content from another Goobers version. Treat links to the default
branch as supplementary. Do not modify repository-root harness instructions
when updating the product-owned `.goobers/agent-toolkit/` directory.
