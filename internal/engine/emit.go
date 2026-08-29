package engine

import (
	"context"
	"fmt"
	"time"

	"go.temporal.io/sdk/temporal"
	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

// ActEmitJournal names the live-journal emission activity
// (distributed-state-and-coordination.md §8, DS4). Must equal the method name
// on Activities.
const ActEmitJournal = "EmitJournal"

// JournalEmitFailedErrorCode is the journal error code recorded when a live
// emission exhausted its bounded budget — the attempt's infra-classed cause
// (#3361: infrastructure never charges the work budget).
const JournalEmitFailedErrorCode = "journal_emit_failed"

// Emit budget: each EmitJournal dispatch retries transport/service failures
// on Temporal's activity retry policy — the "bounded budget inside the
// activity" the design names — then surfaces as one infra-classed failure to
// the workflow's own budget arithmetic.
const (
	emitAttemptTimeout = 30 * time.Second
	emitMaxAttempts    = 5
)

// JournalEmitter is the emission seam: how a stage-executing process reaches
// the daemon's live journal writer. In-process wiring passes
// *livejournal.Writer directly (the daemon's own emitters bypass HTTP);
// remote workers wire livejournal.HTTPEmitter at the write API's journal
// plane. Both satisfy this one interface, which is what lets the same
// workflow code serve co-located and distributed placements.
type JournalEmitter interface {
	Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error)
}

// EmitJournal forwards one batch to the live journal writer. Every failure —
// unreachable daemon, unwired emitter, writer refusal — is classified
// infrastructure: journal emission is never the stage's own work, so its
// failure must consume the infra budget, not the policy budget (#3361, §8
// failure policy).
func (a *Activities) EmitJournal(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	if a.Journal == nil {
		return livejournal.EmitResponse{}, classifySeamError(invoke.InfrastructureFailure(
			fmt.Errorf("run %s requires live journaling but this worker wires no journal emitter: %w", req.RunID, ErrNotConfigured)))
	}
	resp, err := a.Journal.Emit(ctx, req)
	if err != nil {
		return livejournal.EmitResponse{}, classifySeamError(invoke.InfrastructureFailure(err))
	}
	return resp, nil
}

// emitActivityContext builds the options every EmitJournal dispatch runs
// under: a short per-attempt window and a bounded, backing-off retry policy.
// Unlike stage activities (whose retry orchestration lives in
// dispatchWithRetry so every history attempt maps to a journal attempt), the
// emit retries are deliberately server-side: they are delivery retries of the
// SAME idempotent batch, not new attempts of anything, and the writer's
// dedup keys make each redelivery a no-op.
func emitActivityContext(ctx workflow.Context) workflow.Context {
	return workflow.WithActivityOptions(ctx, workflow.ActivityOptions{
		StartToCloseTimeout:    emitAttemptTimeout,
		ScheduleToStartTimeout: stageScheduleToStart,
		RetryPolicy: &temporal.RetryPolicy{
			InitialInterval:    time.Second,
			BackoffCoefficient: 2,
			MaximumInterval:    30 * time.Second,
			MaximumAttempts:    emitMaxAttempts,
		},
	})
}

// journalMark snapshots the accumulator so an attempt whose outcome could not
// be journaled can be rolled back to its last durably-emitted boundary — "an
// effect that cannot be journaled did not happen" (§8), applied to the
// workflow's own projection state so the accumulator and the live journal
// never disagree about which attempts exist.
type journalMark struct {
	ops            int
	branchRecorded bool
}

func (r *runJournal) mark() journalMark {
	return journalMark{ops: len(r.proj.Ops), branchRecorded: r.branchRecorded}
}

// rollbackUnemitted truncates every op accumulated past the mark that the
// live writer has not durably accepted. Ops already emitted are never rolled
// back (they are on disk); the deferred run-branch flag is restored because a
// ref.touched recorded during the attempt is always appended after the
// pre-dispatch emission boundary and therefore always among the truncated
// ops.
func (r *runJournal) rollbackUnemitted(m journalMark) {
	floor := m.ops
	if r.emitted > floor {
		floor = r.emitted
	}
	if len(r.proj.Ops) > floor {
		r.proj.Ops = r.proj.Ops[:floor]
		r.branchRecorded = m.branchRecorded
	}
}

// emitFailure records one exhausted emission budget as an infra-classed error
// attributed to the attempt. AttemptClass infra keeps it out of the
// conformance view (§3.3) — a local run has no journal plane and therefore no
// analogue of this event.
func (r *runJournal) emitFailure(ctx workflow.Context, stage string, attempt int, err error) {
	r.append(ctx, journal.Event{
		Type: journal.EventError, Stage: stage, Attempt: attempt, AttemptClass: journal.AttemptInfra,
		Error:  &journal.ErrorDetail{Code: JournalEmitFailedErrorCode, Message: err.Error()},
		Runner: map[string]any{"retryFailureClass": string(journal.AttemptInfra)},
	})
}

// assignEmitKeys stamps an idempotency key onto every not-yet-keyed op:
// (runID, branch, scope, attempt, ordinal), where scope is the stage or gate
// the op belongs to (empty for run-level ops) and ordinal counts prior keyed
// ops in the same (branch, scope, attempt) group. All inputs are plain
// workflow state, so replay derives identical keys; a repass is a new attempt
// and keys as one (§8). Keys are assigned exactly once — a batch that failed
// to deliver re-emits the SAME keys — and ordinal gaps left by rolled-back
// ops are harmless because keys only need determinism and uniqueness.
func (r *runJournal) assignEmitKeys() {
	if r.ordinals == nil {
		r.ordinals = map[string]int{}
	}
	for i := r.emitted; i < len(r.proj.Ops); i++ {
		if r.proj.Ops[i].EmitKey != "" {
			continue
		}
		scope, attempt := opEmitScope(r.proj.Ops[i])
		group := fmt.Sprintf("%d|%s|%d", 0, scope, attempt)
		ordinal := r.ordinals[group]
		r.ordinals[group] = ordinal + 1
		r.proj.Ops[i].EmitKey = fmt.Sprintf("%s|%d|%s|%d|%d", r.proj.Identity.RunID, 0, scope, attempt, ordinal)
	}
}

// opEmitScope resolves the (scope, attempt) an op's idempotency key is
// grouped under: stage ops key by their stage and attempt, gate ops by the
// gate name, and run-level ops by the empty scope. The engine walk is
// sequential, so branch is always the root branch.
func opEmitScope(op JournalOp) (string, int) {
	switch op.Kind {
	case opArtifact:
		if op.Artifact != nil {
			return op.Artifact.Stage, op.Artifact.Attempt
		}
	case opSpan:
		if op.Span != nil {
			return op.Span.Stage, op.Span.Attempt
		}
	case opAppend:
		if op.Event != nil {
			if op.Event.Stage != "" {
				return op.Event.Stage, op.Event.Attempt
			}
			if op.Event.Gate != "" {
				return op.Event.Gate, 0
			}
		}
	}
	return "", 0
}

// emitPending sends every accumulated-but-unemitted op to the live journal
// writer. A no-op for a run without live journaling (RunInput.LiveJournal
// unset) and when nothing is pending. The first batch carries the Open header
// so the writer creates the run journal at first emit. On success the
// emitted watermark advances; on failure it does not — the ops stay pending
// and the caller decides whether the failure fails the attempt (stage
// dispatch), the run (open/gate), or is deferred to the repair projection
// (terminal).
func (r *runJournal) emitPending(ctx workflow.Context) error {
	if !r.live {
		return nil
	}
	pending := len(r.proj.Ops)
	if r.emitted >= pending {
		return nil
	}
	r.assignEmitKeys()
	ops := make([]livejournal.Op, 0, pending-r.emitted)
	for _, op := range r.proj.Ops[r.emitted:pending] {
		ops = append(ops, liveOpFrom(op))
	}
	req := livejournal.EmitRequest{
		RunID:  r.proj.Identity.RunID,
		Gaggle: r.proj.Identity.Gaggle,
		Ops:    ops,
	}
	if r.emitted == 0 {
		req.Open = &livejournal.OpenHeader{
			Identity:   r.proj.Identity,
			Item:       r.proj.Item,
			Graph:      r.proj.Graph,
			Definition: r.proj.Definition,
		}
	}
	var resp livejournal.EmitResponse
	if err := workflow.ExecuteActivity(emitActivityContext(ctx), ActEmitJournal, req).Get(ctx, &resp); err != nil {
		return fmt.Errorf("engine: emit journal ops for run %s: %w", r.proj.Identity.RunID, err)
	}
	r.emitted = pending
	return nil
}

// emitTerminal is the terminal-boundary emission: best-effort by design. The
// terminal event is already durable in the workflow's accumulated projection
// (and therefore in Temporal history), and the run IS closing — failing the
// workflow here would trade the business outcome for a journal write the
// repair projection (DS5) backfills within one reconcile interval anyway.
func (r *runJournal) emitTerminal(ctx workflow.Context) {
	if err := r.emitPending(ctx); err != nil {
		workflow.GetLogger(ctx).Error(
			"live journal terminal emission failed; history remains authoritative and the repair projection backfills the terminal",
			"error", err)
	}
}

// liveOpFrom converts one accumulated JournalOp to the wire op the journal
// plane accepts.
func liveOpFrom(op JournalOp) livejournal.Op {
	out := livejournal.Op{Kind: op.Kind, Key: op.EmitKey, Time: op.Time}
	switch op.Kind {
	case opAppend:
		if op.Event != nil {
			ev := *op.Event
			out.Event = &ev
		}
	case opArtifact:
		if op.Artifact != nil {
			out.Artifact = &livejournal.ArtifactOp{
				Stage: op.Artifact.Stage, Attempt: op.Artifact.Attempt, Class: op.Artifact.Class,
				Name: op.Artifact.Name, Data: op.Artifact.Data, Integrity: op.Artifact.Integrity,
			}
		}
	case opSpan:
		if op.Span != nil {
			out.Span = &livejournal.SpanOp{
				Stage: op.Span.Stage, Attempt: op.Span.Attempt, Class: op.Span.Class,
				Name: op.Span.Name, DataSchema: op.Span.DataSchema, Ref: op.Span.Ref,
			}
		}
	}
	return out
}
