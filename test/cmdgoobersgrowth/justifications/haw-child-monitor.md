# HAW child monitoring composition

The command adds one daemon option binding the existing durable queue, journal
layout, interactive permission service and scrubber to the child monitoring API.
Projection, pagination and authorization live in internal/childmonitor; protocol
handling lives in internal/httpapi. No command-local data store or provider poller
is introduced. This small composition belongs with the other interactive API
owners because their lifetime and configuration are managed by the daemon.
