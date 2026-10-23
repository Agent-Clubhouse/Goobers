package engine

import (
	"fmt"
	"strconv"
	"strings"

	"go.temporal.io/sdk/workflow"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
	"github.com/goobers/goobers/internal/runsemantics"
)

const hitlLiveTerminalChange = "hitl-live-terminal-transition-v1"

// HITLDelivery is visible while the one keyed journal activity retries. The
// router enriches Attempt/LastError from Temporal's pending activity record;
// retries add no workflow commands or per-attempt signals to history.
type HITLDelivery struct {
	RequestID string `json:"requestId"`
	Status    string `json:"status"`
	Attempt   int32  `json:"attempt"`
	LastError string `json:"lastError,omitempty"`
}

type terminalDelivery struct {
	request livejournal.TerminalInterventionRequest
	ops     []JournalOp
}

func (s *hitlSession) commitTerminalIntent(ctx workflow.Context, intent HITLIntent, plan *hitlResumePlan) error {
	if s.rec.owner != nil {
		if err := s.rec.owner.mu.Lock(ctx); err != nil {
			return err
		}
		defer s.rec.owner.mu.Unlock()
	}
	req, err := s.terminalIntent(intent, plan)
	if err != nil {
		return err
	}
	ops := s.rec.reserveTerminalEvents(ctx, req.Events)
	for i, op := range ops {
		req.Events[i] = *op.Event
	}
	s.delivery = &terminalDelivery{request: livejournal.TerminalInterventionRequest{RunID: s.runID, Gaggle: s.rec.proj.Identity.Gaggle, Transition: req}, ops: ops}
	var receipt journal.TerminalInterventionReceipt
	err = workflow.ExecuteActivity(terminalInterventionContext(ctx), ActCommitTerminalIntervention, s.delivery.request).Get(ctx, &receipt)
	if err != nil {
		// Cancellation leaves the keyed delivery for Run's disconnected fence
		// reconciliation; no late retry may resurrect it after that tombstone.
		if ctx.Err() == nil {
			s.delivery = nil
		}
		return err
	}
	s.rec.publishTerminalOps(s.delivery.ops)
	s.delivery = nil
	return nil
}

func (s *hitlSession) terminalIntent(intent HITLIntent, plan *hitlResumePlan) (journal.TerminalIntervention, error) {
	action, stage := "resume", intent.Gate
	switch intent.Kind {
	case HITLRerunStage:
		action, stage = "rerun", intent.Stage
	case HITLResolveEscalation:
		action = intent.Resolution
	}
	fingerprint, err := runsemantics.InterventionFingerprint(runsemantics.InterventionContent{Action: action, RunID: intent.RunID, Stage: stage, Actor: intent.Actor, Decision: intent.Decision, Rationale: intent.Rationale, InstructionAddendum: intent.InstructionAddendum})
	if err != nil {
		return journal.TerminalIntervention{}, err
	}
	marker := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.idempotency", "idempotencyKey": intent.RequestID, "fingerprint": fingerprint, "action": action, "stage": stage}}
	if plan == nil {
		marker.Runner = map[string]any{"kind": HITLEscalationResolutionMarker, "resolution": "deny", "idempotencyKey": intent.RequestID, "fingerprint": fingerprint, "actor": intent.Actor, "rationale": strings.TrimSpace(intent.Rationale)}
	}
	req := journal.TerminalIntervention{RequestID: intent.RequestID, Fingerprint: fingerprint, ExpectedTerminalGeneration: s.generation, ExpectedTerminalSeq: s.terminalSeq, Events: []journal.Event{marker}}
	if plan != nil {
		event := s.resumedEvent(intent, *plan)
		event.Runner = nil
		if intent.Kind == HITLResolveEscalation && event.Decision == "" {
			event.Decision = gateDefaultDecision
		}
		req.Events = append(req.Events, event)
	}
	return req, nil
}

// reserveTerminalEvents consumes deterministic keys without exposing a resume
// in the repair projection before the journal owner confirms it.
func (r *runJournal) reserveTerminalEvents(ctx workflow.Context, events []journal.Event) []JournalOp {
	r.assignEmitKeys()
	ordinals := r.ordinals
	if r.owner != nil {
		ordinals = r.owner.ordinals
	}
	ops := make([]JournalOp, len(events))
	for i, event := range events {
		event.Time = workflow.Now(ctx)
		op := JournalOp{Kind: opAppend, Event: &event, Time: event.Time}
		assignJournalKey(r.proj.Identity.RunID, ordinals, &op)
		event.Runner = cloneRunner(event.Runner)
		event.Runner["emitKey"] = op.EmitKey
		ops[i] = op
	}
	return ops
}

func cloneRunner(in map[string]any) map[string]any {
	out := make(map[string]any, len(in)+1)
	for key, value := range in {
		out[key] = value
	}
	return out
}

func (r *runJournal) publishTerminalOps(ops []JournalOp) {
	for _, op := range ops {
		r.proj.Ops = append(r.proj.Ops, op)
		r.trackOwnedOp()
	}
	r.emitted = len(r.proj.Ops)
	if r.owner != nil {
		r.owner.emitted = len(r.owner.entries)
		r.owner.submitted = r.owner.emitted
	}
}

func (s *hitlSession) confirmTerminalBoundary(ctx workflow.Context) error {
	index := len(s.rec.proj.Ops) - 1
	s.rec.proj.Ops[index].Receipt = true
	if err := s.rec.emitPending(ctx); err != nil {
		return err
	}
	key := s.rec.proj.Ops[index].EmitKey
	receipts := s.rec.receipts
	if s.rec.owner != nil {
		receipts = s.rec.owner.receipts
	}
	receipt, ok := receipts[key]
	if !ok || receipt.Seq == 0 {
		return fmt.Errorf("engine: missing terminal receipt for %s", key)
	}
	s.terminalSeq = receipt.Seq
	return nil
}

func (s *hitlSession) awaitTerminalIntent(ctx workflow.Context) (bool, error) {
	for {
		if s.pending != nil {
			return true, nil
		}
		if s.delivery != nil {
			if err := workflow.Await(ctx, func() bool { return s.delivery == nil }); err != nil {
				return false, err
			}
			continue
		}
		remaining := s.deadline.Sub(workflow.Now(ctx))
		if remaining <= 0 {
			return false, nil
		}
		ok, err := workflow.AwaitWithTimeout(ctx, remaining, func() bool { return s.pending != nil || s.delivery != nil })
		if err != nil || !ok {
			return false, err
		}
	}
}

// cancelTerminalDelivery reconciles an uncertain Update before Run's ordinary
// abort emission. The journal seam atomically aborts or tombstones the intent;
// the original activity may reply late but can never reopen after this fence.
func (s *hitlSession) cancelTerminalDelivery(ctx workflow.Context) (bool, error) {
	if !s.liveTerminal || s.delivery == nil {
		return false, nil
	}
	if s.rec.owner != nil {
		if err := s.rec.owner.mu.Lock(ctx); err != nil {
			return true, err
		}
		defer s.rec.owner.mu.Unlock()
	}
	pending := s.delivery
	req := pending.request
	marker := journal.Event{Type: journal.EventRunnerAnnotation, Runner: map[string]any{"kind": "intervention.cancelled", "idempotencyKey": req.Transition.RequestID, "fingerprint": req.Transition.Fingerprint, "terminalGeneration": strconv.FormatUint(req.Transition.ExpectedTerminalGeneration, 10), "terminalSeq": strconv.FormatUint(req.Transition.ExpectedTerminalSeq, 10)}}
	ops := s.rec.reserveTerminalEvents(ctx, []journal.Event{marker, {Type: journal.EventRunFinished, Status: string(journal.PhaseAborted), Disposition: journal.RunDispositionProduced, Reason: "operator cancellation"}})
	for _, op := range ops {
		req.Transition.Cancellation = append(req.Transition.Cancellation, *op.Event)
	}
	var receipt journal.TerminalInterventionReceipt
	if err := workflow.ExecuteActivity(terminalInterventionContext(ctx), ActCommitTerminalIntervention, req).Get(ctx, &receipt); err != nil {
		return true, err
	}
	if receipt.CommittedPrefix < 0 || receipt.CommittedPrefix > len(pending.ops) {
		return true, fmt.Errorf("engine: invalid cancelled prefix length %d", receipt.CommittedPrefix)
	}
	s.rec.publishTerminalOps(pending.ops[:receipt.CommittedPrefix])
	s.rec.publishTerminalOps(ops)
	s.delivery = nil
	s.pending = nil
	s.noteTerminal()
	return true, nil
}

// takeHumanRerun keeps attempt identity separate from retry budget accounting:
// the human attempt may be number 12 but still starts a fresh policy budget.
func (r *runJournal) takeHumanRerun(stage string) (int32, journal.AttemptClass) {
	if attempt, ok := r.humanReruns[stage]; ok {
		delete(r.humanReruns, stage)
		return int32(attempt), journal.AttemptHuman
	}
	return 1, ""
}

// Local runGate clears empty-diff short circuits for an explicit reviewer
// addendum, and resetRerunGateSeeds clears its prior diff. Keep that intent
// authoritative after the versioned terminal commit on the cloud path too.
func (r *runJournal) prepareHumanGateRerun(stage string, evidence *gateEvidence, priorDiff *string) {
	if _, ok := r.humanReruns[stage]; !ok {
		return
	}
	delete(r.humanReruns, stage)
	evidence.CachedVerdict = nil
	evidence.CacheHit = false
	evidence.RequireDiff = false
	*priorDiff = ""
}
