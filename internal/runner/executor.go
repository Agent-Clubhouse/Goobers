// Package runner is the substrate-neutral local runner core (ARCHITECTURE.md
// §3.1): it advances a compiled workflow.Machine (#9) stage-by-stage, durably
// records every transition to the run journal (#8), and dispatches each task
// through the pre-existing internal/invoke seam (Deterministic/Goober) — the
// same interface #18's shell executor and #19's agentic executor implement
// against, already wired at internal/engine/activities.go. Gate dispatch (#20)
// goes through internal/gate.Evaluator, which wraps that same invoke.Automated/
// invoke.Goober boundary.
//
// This file defines the one small seam of its own the runner needs on top of
// invoke.*: ArtifactRecorder, and the per-run construction factories, because
// a task executor (e.g. internal/executor.ShellExecutor) binds its journal
// reference at CONSTRUCTION time, not per call — so it must be built fresh for
// each run's own *journal.Run, never shared across runs.
package runner

import (
	"context"
	"fmt"

	"github.com/goobers/goobers/internal/stageexec"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// Shared executor contracts keep local callers source-compatible.
type ArtifactRecorder = stageexec.ArtifactRecorder
type SecretRegistrar = stageexec.SecretRegistrar
type NewDeterministicFunc = stageexec.NewDeterministicFunc
type NewAgenticFunc = stageexec.NewAgenticFunc
type NewAgenticWithMCPServersFunc = stageexec.NewAgenticWithMCPServersFunc

type assetBundleGoober interface {
	HasAssetBundle() bool
}

// gooberInvocation records whether an executor was actually invoked. The
// runner uses this with assetBundleGoober to reserve .goober-assets only for a
// call that materializes a real bundle.
type gooberInvocation struct {
	invoke.Goober
	activateAssetPathGuard func() error
	operatorMessageAddress string
	operatorMessageJournal executionJournal
	operatorMessageRunID   string
	operatorMessageStage   string
	operatorMessageAttempt int
	operatorMessageAgent   string
	invoked                bool
}

func (g *gooberInvocation) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if err := g.prepare(); err != nil {
		return apiv1.ResultEnvelope{}, err
	}
	g.invoked = true
	defer g.registerOperatorMessageTarget()()
	return g.Goober.Invoke(ctx, env)
}

func (g *gooberInvocation) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	// One wrapper serves all retries of a gate. Rebind the delivery target to
	// THIS durable dispatch, including a repeated number on a later visit.
	if g.operatorMessageJournal != nil && env.Attempt > 0 {
		g.operatorMessageAttempt = int(env.Attempt)
		g.operatorMessageAddress = operatorMessageAddressForAttempt(g.operatorMessageJournal, g.operatorMessageRunID, g.operatorMessageStage, int(env.Attempt), g.operatorMessageAgent)
	}
	if err := g.prepare(); err != nil {
		return apiv1.Verdict{}, err
	}
	g.invoked = true
	defer g.registerOperatorMessageTarget()()
	return g.Goober.Review(ctx, env)
}

func (g *gooberInvocation) prepare() error {
	assets, ok := g.Goober.(assetBundleGoober)
	if !ok || !assets.HasAssetBundle() || g.activateAssetPathGuard == nil {
		return nil
	}
	if err := g.activateAssetPathGuard(); err != nil {
		return fmt.Errorf("activate goober asset path guard: %w", err)
	}
	return nil
}

func (g *gooberInvocation) materializedAssets() bool {
	assets, ok := g.Goober.(assetBundleGoober)
	return g.invoked && ok && assets.HasAssetBundle()
}

func (g *gooberInvocation) registerOperatorMessageTarget() func() {
	target, ok := g.Goober.(invoke.OperatorMessageTarget)
	if !ok || g.operatorMessageAddress == "" {
		if !ok || g.operatorMessageJournal == nil {
			return func() {}
		}
		g.operatorMessageAddress = operatorMessageAddressFromStartedSeq(g.operatorMessageRunID, g.operatorMessageStage, g.operatorMessageAttempt, g.operatorMessageAgent, g.operatorMessageJournal.Seq())
		if g.operatorMessageAddress == "" {
			g.operatorMessageAddress = operatorMessageAddressForAttempt(g.operatorMessageJournal, g.operatorMessageRunID, g.operatorMessageStage, g.operatorMessageAttempt, g.operatorMessageAgent)
		}
		if g.operatorMessageAddress == "" {
			return func() {}
		}
	}
	return DefaultOperatorMessageDeliveryRegistry.RegisterWithJournal(g.operatorMessageAddress, target, g.operatorMessageJournal)
}

func newGooberInvocation(goober invoke.Goober, activateAssetPathGuard func() error, jr executionJournal, runID, stage string, attempt int, agent string) *gooberInvocation {
	return &gooberInvocation{
		Goober:                 goober,
		activateAssetPathGuard: activateAssetPathGuard,
		operatorMessageAddress: operatorMessageAddressForAttempt(jr, runID, stage, attempt, agent),
		operatorMessageJournal: jr,
		operatorMessageRunID:   runID,
		operatorMessageStage:   stage,
		operatorMessageAttempt: attempt,
		operatorMessageAgent:   agent,
	}
}

func operatorMessageAddressForAttempt(jr executionJournal, runID, stage string, attempt int, agent string) string {
	reader, err := journal.OpenRead(jr.Dir())
	if err != nil {
		return ""
	}
	events, err := reader.Events()
	if err != nil {
		return ""
	}
	var startedSeq uint64
	branch := 0
	if scoped, ok := jr.(*branchJournal); ok {
		branch = scoped.branch
	}
	for _, event := range events {
		if event.Branch != branch {
			continue
		}
		switch {
		case (event.Type == journal.EventStageStarted || event.Type == journal.EventReviewerStarted) && event.Stage == stage && event.Attempt == attempt:
			startedSeq = event.Seq
		case event.Type == journal.EventGateStarted && event.Gate == stage && event.RepassAttempt() == attempt:
			startedSeq = event.Seq
		}
	}
	if startedSeq == 0 {
		return ""
	}
	return operatorMessageAddressFromStartedSeq(runID, stage, attempt, agent, startedSeq)
}

func operatorMessageAddressFromStartedSeq(runID, stage string, attempt int, agent string, startedSeq uint64) string {
	if startedSeq == 0 {
		return ""
	}
	address, err := journal.StageAgentAddress(runID, stage, attempt, agent, startedSeq)
	if err != nil {
		return ""
	}
	return address.String()
}
