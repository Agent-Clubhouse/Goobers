package main

import (
	"context"
	"errors"
	"strings"

	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/livejournal"
)

type containedJournalPlane struct {
	httpapi.JournalService
	service *daemonCredentialService
}

func (p containedJournalPlane) Emit(ctx context.Context, req livejournal.EmitRequest) (livejournal.EmitResponse, error) {
	principal, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || (!principal.WorkflowParent && !principal.GeneratedChild) {
		return p.JournalService.Emit(ctx, req)
	}
	if principal.Subject != "run:"+req.RunID {
		return livejournal.EmitResponse{}, parentAuthorityRefusal()
	}

	a, err := p.service.containedAttempt(ctx)
	if err != nil {
		return livejournal.EmitResponse{}, err
	}
	if err = a.active(ctx); err != nil {
		return livejournal.EmitResponse{}, err
	}
	req, err = containedJournalRequest(a.contract, a.digest, req)
	if err != nil {
		return livejournal.EmitResponse{}, err
	}
	ctx = livejournal.WithRequestBlobStore(ctx, a.blobs)

	return p.JournalService.Emit(ctx, req)
}

// Namespace keys and transcript captures by signed physical attempt, while
// preserving the writer's existing per-run handle and transcript state machine.
func parentJournalRequest(a parentAttemptCustody, req livejournal.EmitRequest) (livejournal.EmitRequest, error) {
	return containedJournalRequest(a.contract, a.digest, req)
}

func containedJournalRequest(contract childpod.Contract, digest string, req livejournal.EmitRequest) (livejournal.EmitRequest, error) {
	if req.RunID != contract.Identity.RunID || req.Gaggle != contract.Identity.Gaggle || req.Open != nil {
		return req, parentAuthorityRefusal()
	}
	req.Ops = append([]livejournal.Op(nil), req.Ops...)
	for i, op := range req.Ops {
		op = normalizeContainedJournalStage(op, contract.Identity.RunID, contract.Stage, contract.Attempt)
		if err := containedJournalOp(contract, op); err != nil {
			return req, err
		}
		if op.Checkpoint != nil {
			cp := *op.Checkpoint
			suffix, ok := strings.CutPrefix(op.Key, cp.Capture+"/")
			if !ok {
				return req, errors.New("parent transcript key differs from capture")
			}
			cp.Capture = strings.TrimPrefix(journal.Digest([]byte(digest+"/"+cp.Capture)), "sha256:")[:32]
			op.Checkpoint = &cp
			op.Key = cp.Capture + "/" + suffix
		} else {
			op.Key = "contained/" + strings.TrimPrefix(digest, "sha256:") + "/" + op.Key
		}
		req.Ops[i] = op
	}
	return req, nil
}

// Harness observations use either the task ID or bare stage. Only the exact
// contract run prefix is normalized; another run's task ID remains invalid.
func normalizeContainedJournalStage(op livejournal.Op, run, stage string, attempt int) livejournal.Op {
	normalize := func(value string) string {
		if value == run+":"+stage {
			return stage
		}
		return value
	}
	if op.Event != nil {
		copy := *op.Event
		copy.Stage = normalize(copy.Stage)
		if copy.Attempt == 0 {
			copy.Attempt = attempt
		}
		op.Event = &copy
	}
	if op.Artifact != nil {
		copy := *op.Artifact
		copy.Stage = normalize(copy.Stage)
		op.Artifact = &copy
	}
	if op.Span != nil {
		copy := *op.Span
		copy.Stage = normalize(copy.Stage)
		op.Span = &copy
	}
	if op.Checkpoint != nil {
		copy := *op.Checkpoint
		copy.Stage = normalize(copy.Stage)
		op.Checkpoint = &copy
	}
	return op
}

func containedJournalOp(contract childpod.Contract, op livejournal.Op) error {
	stage, attempt := "", 0
	switch {
	case op.Event != nil:
		stage, attempt = op.Event.Stage, op.Event.Attempt
	case op.Artifact != nil:
		stage, attempt = op.Artifact.Stage, op.Artifact.Attempt
	case op.Span != nil:
		stage, attempt = op.Span.Stage, op.Span.Attempt
	case op.Checkpoint != nil:
		stage, attempt = op.Checkpoint.Stage, contract.Attempt
	default:
		return errors.New("parent journal operation has no attempt identity")
	}
	if stage != contract.Stage || attempt != contract.Attempt {
		return errors.New("parent journal operation names another stage attempt")
	}
	// Ordinary telemetry keys are namespaced above; only captures need their
	// writer-defined key shape. Neither can collide with a sibling contract.
	if op.Key == "" || len(op.Key) > 2048 {
		return errors.New("parent journal operation key is invalid")
	}
	return nil
}

// Both roles share transport validation, while their selectors separately prove
// parent-run custody or accepted child lineage from the signed contract.
type containedAttemptCustody struct {
	contract childpod.Contract
	digest   string
	blobs    interface {
		blobstore.Store
		blobstore.BoundedReader
	}
	active func(context.Context) error
	review bool
}

func (s *daemonCredentialService) containedAttempt(ctx context.Context) (containedAttemptCustody, error) {
	p, ok := httpapi.PrincipalFromContext(ctx)
	if !ok {
		return containedAttemptCustody{}, parentAuthorityRefusal()
	}
	if p.WorkflowParent {
		a, err := s.parentAttempt(ctx)
		if err != nil {
			return containedAttemptCustody{}, err
		}
		return containedAttemptCustody{contract: a.contract, digest: a.digest, blobs: a.blobs, active: a.active}, nil
	}
	a, err := s.childAttempt(ctx)
	if err != nil {
		return containedAttemptCustody{}, err
	}
	return containedAttemptCustody{contract: a.contract, digest: a.digest, blobs: a.blobs, active: a.active, review: a.review}, nil
}
