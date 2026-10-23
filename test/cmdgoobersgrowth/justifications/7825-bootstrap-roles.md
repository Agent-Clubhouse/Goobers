# HA-P3-25: bootstrap provisions roles

Growth: +47 non-test lines in `cmd/goobers-cloud`, no new files.

`goobers-cloud bootstrap` gains the administrator connection inputs
(`--admin-url`, `--admin-url-file`, repeatable `--entra-principal`) and the
lazy connection factories that hand the administrator and owner identities to
the bootstrap steps. Provisioning itself is `internal/cloud/pgroles.Provision`
and the step sequencing is `internal/cloud/cutover`; the command file only
parses flags, reads the Secret-mounted URLs through the existing helpers and
opens connections, which the command package already owns.
