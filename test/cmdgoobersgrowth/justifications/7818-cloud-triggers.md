# #7818: cloud PG trigger plane composition

Growth: one production file in `cmd/goobers-cloud` (`controltriggers.go`) and a
single line in the control assembly manifest.

The trigger plane itself (admission, pinning, acceptance, status) lives in
`internal/cloud/triggers` over `internal/cloud/acceptance`. The command package
only composes it: it hands the API role's instance-bound acceptance store to the
service and resolves the process-wide Configuration lazily, which are
process-assembly facts that the command package already owns.
