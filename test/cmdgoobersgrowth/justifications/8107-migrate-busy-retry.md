# Migrate role retries a busy database (#8107)

`goobers-cloud migrate apply` runs as a Job with `backoffLimit: 0` that Argo
does not retry, so the role itself retries a migration refused by lock
contention (`ErrMigrationBusy`) with bounded exponential backoff inside the
Job's deadline, and exits 75 (EX_TEMPFAIL) rather than 1 when the database
stays busy, so a deploy can tell contention from a broken migration. The lock
timeout and the busy classification live in internal/cloud/migrate; the role
adds only the retry loop, the exit code and its help text.
