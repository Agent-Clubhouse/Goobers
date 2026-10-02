package engine

import (
	"errors"
	"fmt"

	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
	"github.com/goobers/goobers/internal/telemetry"
)

// Payload changes affect live emission activity arguments. Old histories keep
// their original projection; new runs pin the additive terminal record.
const terminalCauseChange = "structured-terminal-cause-v1"

func (r *runJournal) failureTerminalCause(stage, code, message string) {
	prior := r.terminalCause
	c := &journal.TerminalCause{Schema: journal.TerminalCauseSchema, Phase: journal.PhaseFailed, Classification: journal.TerminalInfrastructureFailure, SelectorKind: "condition", Code: code, Message: message, CausalEventSeq: uint64(len(r.proj.Ops))}
	if c.Code == "" {
		c.Code = "run_failed"
	}
	if stage != "" {
		c.SelectorKind, c.Selector = "stage", stage
		if !telemetry.ClassifyError(code).InfraFault() {
			c.Classification = journal.TerminalStageFailure
		}
	}
	if prior != nil {
		c.SelectorKind, c.Selector, c.Classification, c.Retry = prior.SelectorKind, prior.Selector, prior.Classification, prior.Retry
	}
	r.terminalCause = c
}

func (r *runJournal) buildTerminalCause(phase journal.RunPhase, finalState string) (*journal.TerminalCause, error) {
	if phase == journal.PhaseCompleted {
		return nil, nil
	}
	// One projection op becomes one journal event. Live journals can contain
	// additional progress/transcript events; CausalEmitKey resolves that offset.
	events := make([]journal.Event, 0, len(r.proj.Ops))
	artifacts := map[string][]byte{}
	refs := map[string]journal.Ref{}
	for i, op := range r.proj.Ops {
		if op.Kind == opArtifact && op.Artifact != nil {
			ref, err := journal.ArtifactRef(op.Artifact.Data)
			if err != nil {
				return nil, err
			}
			refs[op.Artifact.Name] = ref
			artifacts[ref.Digest] = op.Artifact.Data
		}
		if op.Event == nil {
			continue
		}
		e := *op.Event
		e.Schema = journal.EventSchema
		e.Seq = uint64(i + 1)
		if e.Type == journal.EventGateEvaluated && e.Name != "" {
			ref, ok := refs[e.Name]
			if !ok {
				return nil, fmt.Errorf("engine: terminal gate artifact %q missing", e.Name)
			}
			e.Ref = &ref
		}
		events = append(events, e)
	}
	c, err := runner.BuildTerminalCause(events, phase, finalState, r.terminalCause, r.machine, r.proj.Identity.RunControls, func(ref journal.Ref) ([]byte, error) {
		data, ok := artifacts[ref.Digest]
		if !ok {
			return nil, errors.New("engine: terminal verdict artifact missing")
		}
		return data, nil
	})
	if err != nil || c == nil {
		return c, err
	}
	if r.live && c.CausalEventSeq > 0 && c.CausalEventSeq <= uint64(len(r.proj.Ops)) {
		r.assignEmitKeys()
		c.CausalEmitKey = r.proj.Ops[c.CausalEventSeq-1].EmitKey
	}
	return c, nil
}

func (r *runJournal) exhaustedTerminalRetry(stage string, class journal.AttemptClass, attempts, limit int) {
	classification := journal.TerminalInfrastructureFailure
	if class == journal.AttemptPolicy && limit > 1 {
		classification = journal.TerminalRetryExhaustion
	}
	r.terminalCause = &journal.TerminalCause{SelectorKind: "stage", Selector: stage, Classification: classification, Retry: &journal.TerminalBudget{Consumed: max(0, attempts-1), Allowed: max(0, limit-1)}}
}
