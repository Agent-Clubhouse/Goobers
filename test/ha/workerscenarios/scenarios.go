// Package workerscenarios defines the worker-specific nightly HA assertions.
// The kind harness supplies the fixture operations; this package owns the
// no-silent-pass evidence checks.
package workerscenarios

import (
	"context"
	"errors"
	"fmt"
)

const (
	// Chaos13 identifies the worker-eviction adoption scenario.
	Chaos13 = "CHAOS-13"
	// Upgrade11 identifies the held-run rainbow-upgrade scenario.
	Upgrade11 = "UPGRADE-11"
)

// SideEffectObservation is durable custody observed around worker eviction.
type SideEffectObservation struct {
	RunID           string
	Attempt         int
	StagePod        string
	Adopted         bool
	ChargedAttempts int
	Terminal        bool
}

// HITLObservation is durable resolution state around a rainbow rollout.
type HITLObservation struct {
	RunID              string
	HoldID             string
	WorkerBuild        string
	AppliedResolutions int
	Terminal           bool
}

// Fixture is implemented by the hosted kind harness. Every operation must
// return durable observations, not only successful command exit statuses.
type Fixture interface {
	StartSideEffectingStage(context.Context) (SideEffectObservation, error)
	EvictOwningWorker(context.Context, SideEffectObservation) error
	WaitForSideEffectingStage(context.Context, string) (SideEffectObservation, error)

	StartHITLHold(context.Context) (HITLObservation, error)
	RolloutWorkerBuild(context.Context, string) error
	ResolveHITL(context.Context, HITLObservation) (HITLObservation, error)
}

// RunCHAOS13 proves worker eviction adopts the original stage pod without
// charging another attempt.
func RunCHAOS13(ctx context.Context, fixture Fixture) error {
	if fixture == nil {
		return errors.New("CHAOS-13: fixture is required")
	}
	before, err := fixture.StartSideEffectingStage(ctx)
	if err != nil {
		return err
	}
	if before.RunID == "" || before.Attempt < 1 || before.StagePod == "" {
		return fmt.Errorf("CHAOS-13: incomplete initial observation: %+v", before)
	}
	if err := fixture.EvictOwningWorker(ctx, before); err != nil {
		return err
	}
	after, err := fixture.WaitForSideEffectingStage(ctx, before.RunID)
	if err != nil {
		return err
	}
	switch {
	case !after.Adopted:
		return fmt.Errorf("CHAOS-13: stage was not adopted: %+v", after)
	case after.StagePod != before.StagePod:
		return fmt.Errorf("CHAOS-13: second stage pod %q replaced %q",
			after.StagePod, before.StagePod)
	case after.Attempt != before.Attempt:
		return fmt.Errorf("CHAOS-13: attempt changed %d -> %d",
			before.Attempt, after.Attempt)
	case after.ChargedAttempts != before.ChargedAttempts:
		return fmt.Errorf("CHAOS-13: attempt charge changed %d -> %d",
			before.ChargedAttempts, after.ChargedAttempts)
	case !after.Terminal:
		return fmt.Errorf("CHAOS-13: adopted stage did not settle: %+v", after)
	default:
		return nil
	}
}

// RunUPGRADE11 proves a held run survives a worker build rollout and an exact
// duplicate resolution is applied once.
func RunUPGRADE11(
	ctx context.Context,
	fixture Fixture,
	nextBuild string,
) error {
	if fixture == nil || nextBuild == "" {
		return errors.New("UPGRADE-11: fixture and next build are required")
	}
	held, err := fixture.StartHITLHold(ctx)
	if err != nil {
		return err
	}
	if held.RunID == "" || held.HoldID == "" || held.WorkerBuild == "" {
		return fmt.Errorf("UPGRADE-11: incomplete hold observation: %+v", held)
	}
	if err := fixture.RolloutWorkerBuild(ctx, nextBuild); err != nil {
		return err
	}
	first, err := fixture.ResolveHITL(ctx, held)
	if err != nil {
		return err
	}
	second, err := fixture.ResolveHITL(ctx, held)
	if err != nil {
		return err
	}
	switch {
	case first.WorkerBuild == held.WorkerBuild:
		return fmt.Errorf("UPGRADE-11: hold resumed on retired build %q",
			first.WorkerBuild)
	case first.WorkerBuild != nextBuild || second.WorkerBuild != nextBuild:
		return fmt.Errorf("UPGRADE-11: resolution builds = %q/%q, want %q",
			first.WorkerBuild, second.WorkerBuild, nextBuild)
	case first.AppliedResolutions != 1 || second.AppliedResolutions != 1:
		return fmt.Errorf("UPGRADE-11: resolution applied counts = %d/%d",
			first.AppliedResolutions, second.AppliedResolutions)
	case !second.Terminal:
		return fmt.Errorf("UPGRADE-11: resolved run did not settle: %+v", second)
	default:
		return nil
	}
}
