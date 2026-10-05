# HAW-EVT-001: event receipt maintenance

The daemon coordination sweep grows by four statements to invoke bounded event
receipt maintenance, including while the scheduler is unavailable. It is the
existing owner of the shared trigger database and maintenance clock. Envelope
validation, durable acceptance, replay identity, reservations and pruning remain
in `internal/eventing` and `internal/triggerqueue`; no new command implementation
or second database is introduced. The sweep integration prevents an accumulating
store from shipping with only a test-only retention function.
