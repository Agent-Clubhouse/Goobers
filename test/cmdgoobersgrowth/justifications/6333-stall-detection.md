# cmd/goobers growth: issue #6333

The opt-in stall-detection observer needs two lines in `cmd/goobers/runnerwiring.go`
to hand the observer to the local agentic stage. All detection logic lives in
`internal/decisiongate` and `internal/runtimewiring`; the command only wires it.
