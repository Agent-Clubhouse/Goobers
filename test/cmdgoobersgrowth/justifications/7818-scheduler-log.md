# #7818: shared per-epoch scheduler log opener

Growth: +91 non-test lines and +1 production file in `cmd/goobers-cloud`
(`controlschedulerlog.go`).

Opening the scheduler stream takes its owner token, so a second open in the same
lease epoch is refused. The scheduler admission and the terminal hooks both need
the same fenced instance log, so the composition root hands every consumer of an
epoch one incarnation and retires it exactly once when the epoch ends. The
behaviour is process assembly (it joins the journal backend to its two
consumers), and it has no home in `internal/` because it is keyed by the
controller's epoch handle and the role's reporter; the log itself is
`internal/cloud/journalpg`.
