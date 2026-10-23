# cmd/goobers-cloud growth: HA-P3 group D (provider observations)

#7818. `cmd/goobers-cloud/controlobservations.go` is the composition-root binder for the provider-observation seam: `observationsFor(state, report)` turns the scheduler's `ProviderQuotaState` into `epochWiring.Observations`. The seam stays pending (the epoch refuses, naming it) until the L1 scheduler calls it with its own ledger. The behaviour lives in `internal/cloud/providerobs`; the cmd file is the ~15-line binding that must name both the epoch seam and the ledger.
