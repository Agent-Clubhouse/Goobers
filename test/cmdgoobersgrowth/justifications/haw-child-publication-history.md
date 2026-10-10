# HAW-C07 child publication history composition

The command adapter adds a read-only bridge from the daemon-owned trigger store
through childpublication.Inspect into the read service. One LocalSources binding
connects it after daemon initialization. Inspection verifies retained custody;
the adapter maps only safe display fields and the exact source run identity.
Validation, pagination and presentation remain in internal/readservice and Portal.
The adapter acquires no credentials and performs no provider calls or writes.
