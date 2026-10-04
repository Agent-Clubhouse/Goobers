package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/httpapi"
)

type containedSurrenderPlane struct {
	*dispatcher.SurrenderDir
	service *daemonCredentialService
}

func (p containedSurrenderPlane) Put(ctx context.Context, run, stage string, attempt int, data []byte) error {
	principal, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || !principal.WorkflowParent {
		return p.SurrenderDir.Put(ctx, run, stage, attempt, data)
	}
	a, err := p.service.parentAttempt(ctx)
	if err != nil {
		return err
	}
	if run != a.contract.Identity.RunID || stage != a.contract.Stage || attempt != a.contract.PodAttempt {
		return parentAuthorityRefusal()
	}
	if err = a.active(ctx); err != nil {
		return err
	}
	if err = validateParentSurrender(ctx, a, data); err != nil {
		return err
	}
	return p.SurrenderDir.Put(ctx, run, stage, attempt, data)
}

func validateParentSurrender(ctx context.Context, a parentAttemptCustody, data []byte) error {
	var out dispatcher.SurrenderedResult
	if len(data) > 1<<20 {
		return errors.New("parent surrender exceeds bound")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&out); err != nil {
		return err
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return errors.New("parent surrender must contain one document")
	}
	if out.Validate() != nil || out.ChildWorkspaceDigest == "" || !out.RecoveryAcknowledged || out.WorkspaceDelta != "" || out.WorkspaceDeltaUnchanged || out.WorkspaceDeltaBase != "" || out.WorkspaceDeltaTip != "" || len(out.Mutations) != 0 || len(out.MutationIssues) != 0 || out.Result.WorkspaceRevision != nil || out.Verdict != nil {
		return errors.New("parent surrender exceeds contained execution authority")
	}
	raw, err := a.blobs.Get(ctx, out.ChildWorkspaceDigest)
	if err != nil {
		return err
	}
	if _, err = childpod.DecodeOutput(raw, out.ChildWorkspaceDigest, a.digest, a.contract); err != nil {
		return err
	}
	artifacts := append([]apiv1.ArtifactPointer(nil), out.Result.Artifacts...)
	if out.Result.Transcript != nil {
		artifacts = append(artifacts, *out.Result.Transcript)
	}
	for _, artifact := range artifacts {
		raw, err := a.blobs.Get(ctx, artifact.Digest)
		if err != nil {
			return err
		}
		if int64(len(raw)) != artifact.Size {
			return errors.New("parent returned artifact size differs from scoped custody")
		}
	}
	return nil
}
