# OneCollector command adapters

This PR adds 316 non-test lines and two files to `cmd/goobers` for capability
discovery, profile-reference installation, stdin upload, and profile-defined MCP
tools. The command package owns argument parsing, receipts, exit codes, stdio,
and configuration edits that preserve comments through its existing YAML helpers.

Transport, input validation, mappings, signal conversion, buffering, and export
lifecycle remain in `internal/telemetry` and `internal/telemetry/onecollector`.
Credential setup stays in `internal/runtimewiring`. Moving only these command
adapters would duplicate the existing CLI and configuration-writing helpers
without moving the reusable implementation. No shared growth snapshot changes.
