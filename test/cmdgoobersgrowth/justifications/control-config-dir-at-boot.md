# Control validates --config-dir at boot

The control role's `--config-dir` check moves out of
`bindControlConfiguration` into `validateControlConfigDir`. The check now runs
with the rest of control's flag validation, before any database is opened. A
missing directory then exits 2 with its real message instead of exiting 1 as
"database operation failed". The check reads a command-layer flag and gates the
command's own boot order, so it belongs in cmd/goobers-cloud. It adds no flag,
command or lifecycle.
