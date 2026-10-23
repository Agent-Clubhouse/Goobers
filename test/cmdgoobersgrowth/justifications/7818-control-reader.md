# Control-role Reader composition (#7818 R3)

The `goobers-cloud control` role serves its read API from
`internal/cloud/cloudreader`, a rootless read service over the journal plane
and the PostgreSQL read model, rebuilt on every applied configuration
generation. The service, the generation swap, the unavailable-before-first-apply
behaviour and the trigger-plane `Contains` verifier all live below the command
layer.

The command layer keeps only composition: `constructReader`, the
controller-side seam registered after Configuration and Journal. It wires the
Journal seam's reader and projector health, the read-only `ActiveReader` and
the applied configuration, and fails closed without them.

It cannot live below `cmd/goobers-cloud`. Only the role's composition root
holds the per-role database services, the lease owner and the other seams'
outputs together. It adds no command and no second authority path.
