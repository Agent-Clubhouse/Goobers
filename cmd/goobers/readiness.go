package main

import (
	"context"
	"os"
	"time"

	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/readservice"
)

// daemonInstanceReadinessService backs the readiness-gate endpoint (#5019):
// durable root identity read fresh off disk each call (cheap — the daemon is
// the only writer), plus the same startup phase tracker (#4368) and Ready
// gate (#3806) that already back the startup-diagnostic log line and
// /readyz.
//
// It is wired into the router before crash-orphan Reap runs and is the one
// route the recovery gate in httpapi.Router.serve never blocks, so every
// field it reports must stay safe to read the instant the daemon has a
// Layout and a loaded config — well before the read model, active-run
// counts, or any of the other subsystems /api/v1/instance depends on exist.
type daemonInstanceReadinessService struct {
	instanceRoot string
	tracker      *startupPhaseTracker
	ready        func() bool
}

func (s *daemonInstanceReadinessService) InstanceReadiness(context.Context) (httpapi.InstanceReadiness, error) {
	computerName, _ := os.Hostname()
	phase, target, since := s.tracker.snapshot()
	var elapsed time.Duration
	if !since.IsZero() {
		elapsed = time.Since(since)
	}
	return httpapi.InstanceReadiness{
		APIVersion:    readservice.APIVersion,
		SchemaVersion: readservice.SchemaVersion,
		ComputerName:  computerName,
		InstanceRoot:  s.instanceRoot,
		RootIdentity:  readservice.InspectRootIdentity(s.instanceRoot),
		Ready:         s.ready(),
		Recovery: httpapi.InstanceRecoveryPhase{
			Phase:          phase,
			Target:         target,
			ElapsedSeconds: elapsed.Seconds(),
		},
	}, nil
}
