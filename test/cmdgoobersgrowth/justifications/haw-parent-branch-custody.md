# Contained parent branch custody

The command adapters now accept the runner's owned branch journal and bind each
physical pod contract to that branch. Live-stage admission checks only its own
writers; whole-run recovery still requires all writers to be reconciled. The
existing parent recovery adapter processes independently held branch workspaces
in durable attempt order and rejects shared workspace custody. Generic bounded
recorder ownership remains in `internal/runner`; this command code joins daemon
source archives, worker transport and signed parent custody without adding a new
Git engine or Kubernetes client.
