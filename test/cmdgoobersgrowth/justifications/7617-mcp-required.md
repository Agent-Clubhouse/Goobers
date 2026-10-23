# Compile DSL 3.1 MCP requiredness into runtime wiring (#7617)

Growth: about +29 non-test lines and +0 production files in `cmd/goobers`.

DSL 3.1 makes an omitted `mcpServers[].required` fail closed, while older DSL
pins retain each harness's legacy behavior. The reusable version comparison and
default materialization live in `internal/workflow`, and the runner carries the
compiler-materialized server declarations through its version-aware factory.

The remaining command-package lines are composition boundaries:

- `runnerwiring.go` passes a compiled workflow's effective MCP declarations to
  the existing harness executor constructor while preserving the legacy factory
  used by callers without a compiled catalog.
- `agentickitwriter.go` selects the pinned workflow definition before publishing
  the equivalent materialized Goober spec to an isolated stage pod.

These adapters depend on command-owned instance snapshots, pod kits, credential
composition, and executor construction. Moving them elsewhere would invert
those ownership boundaries rather than create reusable logic.
