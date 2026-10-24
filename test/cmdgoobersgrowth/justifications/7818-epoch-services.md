# #7818: cloud epoch services composition

Growth: +92 non-test lines and +1 production file in `cmd/goobers-cloud`
(`controlepoch.go`), plus two lines in `control.go`.

The epoch behaviour itself (adoption of every older epoch, liveness proof,
renewal, the recovery gate, awaiter lifecycle) lives in `internal/cloud/epoch`.
The command package only composes it: it builds the per-epoch lease fence over
the controller's direct pool, hands the stores and the Temporal client to
`epoch.Build`, and records the parts other PRs supply (claims maintenance,
provider observations, awaiters) so the seam refuses by name until they are
present. Process assembly and the dialed Temporal client are owned by this
package.
