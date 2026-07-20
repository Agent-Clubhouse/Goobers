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

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
)

// ArtifactRecorder persists stage output bytes into the run journal by content
// digest. Mirrors internal/executor.ArtifactRecorder and internal/gate's
// Journal interface — *journal.Run satisfies all three structurally (same
// RecordArtifact method), so the runner hands its run's own journal.Run
// straight through with no adapter.
type ArtifactRecorder interface {
	RecordArtifact(name string, data []byte) (journal.Ref, error)
}

// SecretRegistrar receives every secret a run's executors resolve, so the
// same run's journal scrubber (registered from the identical instance — see
// Start) can redact it from anything written to the journal at rest. This is
// defense-in-depth alongside each executor's own credential-issuing
// redaction (#14, #66): an executor already scrubs its own artifact/result
// content before it reaches the journal, but a runner-authored event (e.g. an
// executor_error message that happens to echo a credential) only ever passes
// through the run's own journal.Scrubber — which is a pattern-net-only
// fallback unless it is chained with the exact-value registry this interface
// feeds. Satisfied directly by journal.DefaultScrubber()'s
// *journal.RegistryScrubber, and by internal/credentials.SecretRegistrar
// callers (identical method shape) — no adapter needed either direction.
type SecretRegistrar interface {
	Register(secret []byte)
}

// NewDeterministicFunc constructs the deterministic-task executor for one run,
// bound to rec so its captured output/result-file artifacts land in that
// run's journal (e.g. internal/executor.NewShellExecutor(injector, rec)), and
// to reg so every credential its internal *credentials.Injector resolves is
// also registered with this run's journal scrubber (see SecretRegistrar).
// Required if the workflow has any deterministic task.
type NewDeterministicFunc func(rec ArtifactRecorder, reg SecretRegistrar) (invoke.Deterministic, error)

// NewAgenticFunc constructs the agentic executor for one named goober, bound
// to rec and reg (see NewDeterministicFunc for both). Keyed by goober name
// since a single Runner/run can target more than one distinct goober (e.g.
// "coder" for a task, "reviewer" for its gate); the caller's closure resolves
// that name to its instructions/model/harness config. Required if the
// workflow has any agentic task or agentic gate. The same constructed
// invoke.Goober serves both a Task.Goober's Invoke and a paired AgenticGate's
// Review — one instance, two methods.
type NewAgenticFunc func(gooberName string, rec ArtifactRecorder, reg SecretRegistrar) (invoke.Goober, error)

type assetBundleGoober interface {
	HasAssetBundle() bool
}

// gooberInvocation records whether an executor was actually invoked. The
// runner uses this with assetBundleGoober to reserve .goober-assets only for a
// call that materializes a real bundle.
type gooberInvocation struct {
	invoke.Goober
	invoked bool
}

func (g *gooberInvocation) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	g.invoked = true
	return g.Goober.Invoke(ctx, env)
}

func (g *gooberInvocation) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	g.invoked = true
	return g.Goober.Review(ctx, env)
}

func (g *gooberInvocation) materializedAssets() bool {
	assets, ok := g.Goober.(assetBundleGoober)
	return g.invoked && ok && assets.HasAssetBundle()
}
