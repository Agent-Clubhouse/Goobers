# Schedule-demand archive builder wiring

The daemon now supplies its existing archived runtime builder to both ordinary
starts and demand-sized schedule observations. Sharing the closure adds one
production line in cmd/goobers. The queue, retention, observation and scheduling
logic live in internal/startintent, internal/triggerqueue and
internal/localscheduler. This wiring belongs beside the daemon-owned archive
store and runtime lifecycle; it introduces no new command-package abstraction.
