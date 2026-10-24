# L1a: reset-rate-limit help names the cloud limitation

`goobers reset-rate-limit` writes a marker file under a local instance
directory, which a cloud (goobers-cloud) scheduler never reads. The three added
non-test lines are help text stating that the command is local-instances-only
and that the cloud refuses a reset explicitly (the cloud scheduler log returns
`journalpg.ErrRateResetUnsupported`). No new command, flag, file or logic; the
real cloud admin path is tracked in L1e.
