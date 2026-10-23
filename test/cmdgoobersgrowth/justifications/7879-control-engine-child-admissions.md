# Control-role engine child admissions (#7879 B3b)

The `goobers-cloud control` role now serves generated-child custody to engine
workers. All behaviour lives below the command layer:
- `internal/cloud/childengine.Fenced` binds each call to the live controller
  lease;
- `PinnedResolver` loads the child's pinned generation;
- `childworkflow.PinnedAuthority` and `OfflineGooberAdmission` are shared with
  the daemon;
- `enginechildplane.New` provides the HTTP adapter.

The command layer keeps only composition:

- `constructEngineChildAdmissions`, the seam constructor registered after
  Configuration, which fails closed without the applied configuration, the
  controller database or the object store;
- the lease manager passed to seam constructors, with the instance ID carried
  in `databaseServices`;
- `--object-store-url` / `--object-store-container` and their help lines, plus
  `bindControlObjectStore`, which builds the blob store and controller-pool
  object metadata through the role's existing object-store seams.

None of this can live below `cmd/goobers-cloud`. Only the role's composition
root holds the flags, the per-role database services, the lease manager and
the assembly registry together. It adds no command and no second authority
path.
