# cmd/goobers growth: #6346 named telemetry exporters

Remaining command growth wires named destinations into the daemon, command-journal
and diagnostic lifecycles, and passes CLI probe selection and status output through
the existing commands. The thin constructor callbacks must remain here because
configureOTLP and configureAzureMonitor are cmd-local legacy adapters that own the
credential resolver and registry setup. Flag parsing, exit codes, and stdout/stderr
also belong to those CLI entry points; moving them would require moving unrelated
legacy command implementations.

Destination concurrency, diagnostic filtering, profile tagging, spool selection,
and probe selection live in internal/bootstrap. Deterministic health rendering
lives with its status model in internal/readservice. Transport fan-out, independent
queues and health, replay evidence, configuration validation, and stable spool
identity remain in internal/telemetry and internal/instance. No command or Go
package is added; unchanged acceptance tests exercise the production composition.
