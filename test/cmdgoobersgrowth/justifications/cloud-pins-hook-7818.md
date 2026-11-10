# Cloud config pins hook (#7818)

Growth: +1 non-test line and +0 files in `cmd/goobers-cloud`, in
`controlassembly_config.go`.

## Why the growth belongs in the command package

The line binds the already-built admission-pin computation (`startpin.Compute`,
in `internal/cloud/startpin`) into the control role's `DirSource`. That binding
is composition-root wiring: it names which concrete implementation the control
role hands the config source. All of the behaviour (computing pins from the
extracted tree, refusing an unpinnable generation, naming a missing `config/`
tree) lives in `internal/cloud/config` and `internal/cloud/startpin` and is
tested there.

## Could any of it live elsewhere?

No. A one-line field assignment in the composition root is the smallest form of
this wiring.
