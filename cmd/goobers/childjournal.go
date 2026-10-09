package main

import (
	"context"
	"errors"
	"net/http"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type childJournalPlane struct {
	writer  httpapi.JournalService
	service *daemonCredentialService
}

func (p childJournalPlane) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	if p.writer == nil || p.service == nil {
		return livejournal.EmitResponse{}, childJournalRefusal()
	}
	a, err := p.service.childAttempt(ctx)
	if err != nil || a.custody(ctx) != nil {
		return livejournal.EmitResponse{}, childJournalRefusal()
	}
	req, err = childJournalRequest(a.contract, a.digest, req)
	if err != nil {
		return livejournal.EmitResponse{}, err
	}
	for _, op := range req.Ops {
		if op.Event != nil && op.Event.Type == journal.EventStageHeartbeat && a.active(ctx) != nil {
			return livejournal.EmitResponse{}, childJournalRefusal()
		}
	}
	ctx = livejournal.WithRequestBlobStore(ctx, a.blobs)
	return p.writer.Emit(livejournal.WithRequestBranch(ctx, a.contract.ChildBranch), req)
}

func childJournalRefusal() error {
	return httpapi.NewInterventionError(http.StatusForbidden, "child_journal_refused", "child journal emission requires exact physical custody and observation-only operations", nil)
}

// Child workers report observations; only the host writes lifecycle, custody,
// authoritative artifacts and interventions. Validate the entire batch before
// passing anything to the writer so a forbidden trailing op cannot partly apply.
func childJournalRequest(c childpod.Contract, digest string, req livejournal.EmitRequest) (livejournal.EmitRequest, error) {
	if req.RunID != c.Identity.RunID || req.Gaggle != c.Identity.Gaggle || req.Open != nil {
		return req, childJournalRefusal()
	}
	req.Ops = append([]livejournal.Op(nil), req.Ops...)
	for i, op := range req.Ops {
		if op.Key == "" || len(op.Key) > 1024 {
			return req, childJournalRefusal()
		}
		var err error
		op, err = childObservationOp(c, op)
		if err != nil {
			return req, err
		}
		if op.Checkpoint != nil {
			cp := *op.Checkpoint
			suffix, ok := strings.CutPrefix(op.Key, cp.Capture+"/")
			if !ok {
				return req, childJournalRefusal()
			}
			cp.Capture = strings.TrimPrefix(journal.Digest([]byte(digest+"/"+cp.Capture)), "sha256:")[:32]
			op.Checkpoint, op.Key = &cp, cp.Capture+"/"+suffix
		} else {
			op.Key = "child/" + strings.TrimPrefix(digest, "sha256:") + "/" + op.Key
		}
		req.Ops[i] = op
	}
	return req, nil
}

func childObservationOp(c childpod.Contract, op livejournal.Op) (livejournal.Op, error) {
	count := 0
	for _, present := range []bool{op.Event != nil, op.Artifact != nil, op.Span != nil, op.Checkpoint != nil} {
		if present {
			count++
		}
	}
	if count != 1 {
		return op, childJournalRefusal()
	}
	normalize := func(stage string) string {
		return stageArtifactName(c.Identity.RunID, stage)
	}
	stage, attempt := "", 0
	switch op.Kind {
	case livejournal.OpAppend:
		if op.Event == nil {
			return op, childJournalRefusal()
		}
		e := *op.Event
		if err := childObservationEvent(e); err != nil {
			return op, err
		}
		stage, attempt = normalize(e.Stage), e.Attempt
		if attempt == 0 {
			attempt = c.Attempt
		}
		// Project only observational fields. A worker cannot smuggle a trusted
		// ref, terminal cause, workspace revision or human actor in an agent event.
		e = journal.Event{Type: e.Type, Stage: stage, Attempt: attempt, Agent: e.Agent, Progress: e.Progress, Error: e.Error, Runner: e.Runner}
		op.Event = &e
	case livejournal.OpArtifact:
		if op.Artifact == nil {
			return op, childJournalRefusal()
		}
		a := *op.Artifact
		if !childObservationIntegrity(a.Integrity) || (a.Ref != nil && !childObservationIntegrity(a.Ref.Integrity)) {
			return op, childJournalRefusal()
		}
		stage, attempt = normalize(a.Stage), a.Attempt
		a.Stage = stage
		op.Artifact = &a
	case livejournal.OpSpan:
		if op.Span == nil || !childObservationIntegrity(op.Span.Ref.Integrity) {
			return op, childJournalRefusal()
		}
		s := *op.Span
		stage, attempt = normalize(s.Stage), s.Attempt
		s.Stage = stage
		op.Span = &s
	case livejournal.OpTranscriptCheckpoint:
		if op.Checkpoint == nil || (op.Checkpoint.FinalRef != nil && !childObservationIntegrity(op.Checkpoint.FinalRef.Integrity)) {
			return op, childJournalRefusal()
		}
		cp := *op.Checkpoint
		stage, attempt = normalize(cp.Stage), c.Attempt
		cp.Stage = stage
		op.Checkpoint = &cp
	default:
		return op, childJournalRefusal()
	}
	if stage != c.Stage || attempt != c.Attempt {
		return op, childJournalRefusal()
	}
	return op, nil
}

func childObservationIntegrity(value apiv1.Integrity) bool {
	return value == "" || value == apiv1.IntegrityDerived || value == apiv1.IntegrityUnapproved
}

func childObservationEvent(e journal.Event) error {
	if e.Branch != 0 {
		return childJournalRefusal()
	}
	switch e.Type {
	case journal.EventAgentLifecycle, journal.EventAgentMessage, journal.EventAgentProgress, journal.EventError, journal.EventStageHeartbeat:
		if len(e.Runner) != 0 {
			return childJournalRefusal()
		}
		return nil
	case journal.EventRunnerIsolationPosture:
		if _, hasKind := e.Runner["kind"]; hasKind {
			return childJournalRefusal()
		}
		return nil
	case journal.EventRunnerAnnotation:
		kind, _ := e.Runner["kind"].(string)
		switch kind {
		case "agent-telemetry-fidelity", "goobers-io-input-inspection-receipts", "credit-span-provenance", "required-mcp-readiness", "mcp-server-unavailable":
			return nil
		}
	}
	return errors.Join(childJournalRefusal(), errors.New("worker cannot author host lifecycle or custody events"))
}
