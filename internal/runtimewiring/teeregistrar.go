package runtimewiring

import (
	"time"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

// TeeRegistrar forwards every registered secret to BOTH a run's own
// SecretRegistrar (feeding that run's journal scrubber) and the instance-global
// shared registry (feeding the span exporter + instance log). It is how a
// per-run secret reaches the two instance-lifetime consumers without changing
// internal/runner's per-run registrar creation (#117 Piece B).
type TeeRegistrar struct {
	Run    runner.SecretRegistrar
	Shared *journal.RegistryScrubber
}

// Register registers secrets with both underlying registrars.
func (t TeeRegistrar) Register(secret []byte) {
	t.Run.Register(secret)
	t.Shared.Register(secret)
}

// RegisterUntil forwards an issuer-stated expiry so the instance-lifetime
// shared registry can retire the value (#2656). The run's own registry is
// bounded by the run's lifetime already, so it keeps the value permanently: a
// parked run that resumes long after the token expired still redacts it.
func (t TeeRegistrar) RegisterUntil(secret []byte, expiresAt time.Time) {
	t.Run.Register(secret)
	t.Shared.RegisterUntil(secret, expiresAt)
}
