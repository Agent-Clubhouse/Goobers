# cmd/goobers-cloud growth: PR #7875

HA-P3-10. `migrate apply` and `bootstrap` now pass `migrate.Runner.Report` so a partition pass deferred by an ATTACH lock timeout is printed as a stderr warning in the Job logs instead of being swallowed. The warning writer itself lives in `internal/cloud/migrate` (`migrate.WarningTo`); what remains in `cmd/goobers-cloud` is the two call-site wirings (one import and one `Report:` field), which must be in the composition root.
