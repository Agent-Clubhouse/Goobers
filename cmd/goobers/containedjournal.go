package main

import (
	"context"
	"errors"
	"strings"

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
	if principal.WorkflowParent {
		a, err := p.service.parentAttempt(ctx)
		if err != nil {
			return livejournal.EmitResponse{}, err
		}
		if err = a.active(ctx); err != nil {
			return livejournal.EmitResponse{}, err
		}
		req, err = parentJournalRequest(a, req)
		if err != nil {
			return livejournal.EmitResponse{}, err
		}
		ctx = livejournal.WithRequestBlobStore(ctx, a.blobs)
	} else {
		id, scoped, err := p.service.childBlobScope(ctx)
		if err != nil {
			return livejournal.EmitResponse{}, err
		}
		if !scoped {
			return livejournal.EmitResponse{}, parentAuthorityRefusal()
		}
		if req.Gaggle != id.Gaggle {
			return livejournal.EmitResponse{}, parentAuthorityRefusal()
		}
		ctx = livejournal.WithRequestBlobStore(ctx, childpod.ScopedBlobs{Queue: p.service.childQueue, Identity: id})
	}
	return p.JournalService.Emit(ctx, req)
}

// Namespace keys and transcript captures by signed physical attempt, while
// preserving the writer's existing per-run handle and transcript state machine.
func parentJournalRequest(a parentAttemptCustody, req livejournal.EmitRequest) (livejournal.EmitRequest, error) {
	if req.RunID != a.contract.Identity.RunID || req.Gaggle != a.contract.Identity.Gaggle || req.Open != nil {
		return req, parentAuthorityRefusal()
	}
	req.Ops = append([]livejournal.Op(nil), req.Ops...)
	for i, op := range req.Ops {
		if err := parentJournalOp(a, op); err != nil {
			return req, err
		}
		if op.Checkpoint != nil {
			cp := *op.Checkpoint
			suffix, ok := strings.CutPrefix(op.Key, cp.Capture+"/")
			if !ok {
				return req, errors.New("parent transcript key differs from capture")
			}
			cp.Capture = strings.TrimPrefix(journal.Digest([]byte(a.digest+"/"+cp.Capture)), "sha256:")[:32]
			op.Checkpoint = &cp
			op.Key = cp.Capture + "/" + suffix
		} else {
			op.Key = "parent/" + strings.TrimPrefix(a.digest, "sha256:") + "/" + op.Key
		}
		req.Ops[i] = op
	}
	return req, nil
}

func parentJournalOp(a parentAttemptCustody, op livejournal.Op) error {
	stage, attempt := "", 0
	switch {
	case op.Event != nil:
		stage, attempt = op.Event.Stage, op.Event.Attempt
	case op.Artifact != nil:
		stage, attempt = op.Artifact.Stage, op.Artifact.Attempt
	case op.Span != nil:
		stage, attempt = op.Span.Stage, op.Span.Attempt
	case op.Checkpoint != nil:
		stage, attempt = op.Checkpoint.Stage, a.contract.Attempt
	default:
		return errors.New("parent journal operation has no attempt identity")
	}
	if stage != a.contract.Stage || attempt != a.contract.Attempt {
		return errors.New("parent journal operation names another stage attempt")
	}
	// Ordinary telemetry keys are namespaced above; only captures need their
	// writer-defined key shape. Neither can collide with a sibling contract.
	if op.Key == "" || len(op.Key) > 2048 {
		return errors.New("parent journal operation key is invalid")
	}
	return nil
}
