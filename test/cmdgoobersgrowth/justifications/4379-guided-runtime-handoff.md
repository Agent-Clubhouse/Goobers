# Guided runtime handoff: #4379

Growth: +145 non-test lines and +0 files in `cmd/goobers`.

The guided setup server now owns the final process handoff after configuration
validation: it presents the foreground command, delegates Windows Scheduled
Task or SCM lifecycle actions to the supported `goobers service` commands,
verifies their reported status, and returns enabled workflow schedules and
manual commands. Foreground mode must also transfer control from the setup HTTP
server to `goobers up` in the original terminal process.

These additions belong in `cmd/goobers` because they coordinate existing CLI
commands, the guided HTTP transport, and the lifetime of the current process.
Reusable supervision and configuration behavior remains in `internal/service`
and `internal/instance`; the guided server does not reimplement either
subsystem. The new branches are covered by the guided server and command tests.
