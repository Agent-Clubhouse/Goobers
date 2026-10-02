# cmd/goobers growth: #6346 named telemetry exporters

The command package gains destination credential resolution and lifecycle wiring,
selected-destination probe handling, and human status rendering. These functions
compose the existing daemon, command journal, credential resolver, and diagnostic
setup, whose ownership is already in cmd/goobers. Transport fan-out, independent
queues and health, replay evidence, configuration validation, and stable spool
identity remain in internal/telemetry and internal/instance. No command or Go
package is added; acceptance tests exercise the production composition here.
