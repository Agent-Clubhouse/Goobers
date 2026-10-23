# cmd/goobers-cloud growth: HA-P3 group D (provider observations)

#7818. `cmd/goobers-cloud/controlobservations.go` composes the controller's provider quota ledger into the epoch seam (`epochWiring.Observations`), the one place the control role's composition root can name the shared `localscheduler.ProviderQuotaState` and the `providerobs` adapter. The behaviour (snapshot encoding, restore ratchet, fencing) lives in `internal/cloud/providerobs` and `internal/cloud/control`; the cmd file is only the 20-line wiring, and the epoch seam refuses to serve without it.
