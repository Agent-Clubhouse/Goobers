// Package scheduling provides the local control plane's claims, scheduler state,
// stalled-run sweep, config-source application and execution-generation services.
// The command owns definitions, live runner registration, reload publication,
// lock telemetry and provider coordination, supplied through interfaces and
// callbacks. This package has no cloud storage dependencies.
package scheduling
