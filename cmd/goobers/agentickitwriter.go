package main

import (
	"github.com/goobers/goobers/internal/runtimewiring"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/agentickit"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/instance"
	"github.com/goobers/goobers/internal/journal"
)

// agentickitwriter.go is the worker half of the agentic claim check.
//
// The worker has the instance configuration; a stage pod does not, and must
// not — that is the same property that stops a stage reading the fleet's
// config. So the worker resolves ONE goober's execution inputs, publishes them
// to the blob plane the pod already reaches, and the dispatcher stamps only the
// resulting digest.
//
// It re-derives from the instance root rather than extending buildRunnerConfig's
// return signature: that function is shared with the daemon, and widening a
// shared constructor to serve one caller is how the two paths start to drift.

// agenticKitWriter satisfies dispatcher.KitWriter.
type agenticKitWriter struct {
	registrar *journal.RegistryScrubber
}

// kitModeFor is the completion contract stamped INTO the kit (decision 001
// rulings 7–8): a reviewer gate attempt (Attempt.Review) owes a verdict,
// everything else a result. The pod learns which from the same verified,
// content-addressed document that carries its instructions — never from a
// pod-spec variable anything with namespace read could set.
func kitModeFor(attempt dispatcher.Attempt) agentickit.Mode {
	if attempt.Review {
		return agentickit.ModeReview
	}
	return agentickit.ModeInvoke
}

func (w agenticKitWriter) buildSnapshotKit(l instance.Layout, snapshot *workerConfigSnapshot, env apiv1.InvocationEnvelope, mode agentickit.Mode) (*agentickit.Kit, error) {
	if snapshot.configDir != "" {
		l = l.WithConfigDir(snapshot.configDir)
	}
	return runtimewiring.BuildAgenticKit(l, snapshot.cfg, snapshot.set, snapshot.instructions, snapshot.skillPackages, env, mode, w.registrar)
}
