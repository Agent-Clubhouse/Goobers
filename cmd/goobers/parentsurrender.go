package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"

	"github.com/goobers/goobers/api/validate"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
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
	return validateContainedSurrender(ctx, a.contract, a.digest, a.blobs, false, data)
}

func validateContainedSurrender(ctx context.Context, contract childpod.Contract, digest string, blobs blobstore.BoundedReader, review bool, data []byte) error {
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
	if out.Validate() != nil || out.ChildWorkspaceDigest == "" || !out.RecoveryAcknowledged || out.WorkspaceDelta != "" || out.WorkspaceDeltaUnchanged || out.WorkspaceDeltaBase != "" || out.WorkspaceDeltaTip != "" || len(out.Mutations) != 0 || len(out.MutationIssues) != 0 || out.Result.WorkspaceRevision != nil || !containedOutputGrade(out.Result.Integrity) || !validContainedVerdict(out, review) {
		return errors.New("parent surrender exceeds contained execution authority")
	}
	raw, err := blobs.GetBounded(ctx, out.ChildWorkspaceDigest, childpod.MaxContractBytes)
	if err != nil {
		return err
	}
	if _, err = childpod.DecodeOutput(raw, out.ChildWorkspaceDigest, digest, contract); err != nil {
		return err
	}
	return validateContainedReturnedPointers(ctx, blobs, out)
}

func validateContainedReturnedPointers(ctx context.Context, blobs blobstore.BoundedReader, out dispatcher.SurrenderedResult) error {
	if len(out.Result.Artifacts) > 128 || (out.Verdict != nil && len(out.Verdict.Evidence) > 128) {
		return errors.New("contained output exceeds pointer bound")
	}
	artifacts := append([]apiv1.ArtifactPointer(nil), out.Result.Artifacts...)
	if out.Result.Transcript != nil {
		artifacts = append(artifacts, *out.Result.Transcript)
	}
	if out.Verdict != nil {
		if err := validateContainedVerdict(*out.Verdict); err != nil {
			return err
		}
		artifacts = append(artifacts, out.Verdict.Evidence...)
	}
	seen := map[string]int64{}
	for _, artifact := range artifacts {
		if !containedOutputGrade(artifact.Integrity) {
			return errors.New("contained output cannot claim source trust")
		}
		if err := artifact.Validate(); err != nil {
			return err
		}
		if size, ok := seen[artifact.Digest]; ok {
			if size != artifact.Size {
				return errors.New("contained artifact has inconsistent sizes")
			}
			continue
		}
		raw, err := blobs.GetBounded(ctx, artifact.Digest, artifact.Size)
		if err != nil {
			return err
		}
		if int64(len(raw)) != artifact.Size {
			return errors.New("returned artifact size differs from scoped custody")
		}
		seen[artifact.Digest] = artifact.Size
	}
	return nil
}

func validContainedVerdict(out dispatcher.SurrenderedResult, review bool) bool {
	if !review || out.Result.Status != apiv1.ResultSuccess {
		return out.Verdict == nil
	}
	return out.Verdict != nil
}

var containedVerdictValidator = sync.OnceValues(validate.New)

func validateContainedVerdict(verdict apiv1.Verdict) error {
	if !verdict.Decision.IsValid() {
		return errors.New("contained reviewer verdict has invalid decision")
	}
	validator, err := containedVerdictValidator()
	if err != nil {
		return err
	}
	raw, err := json.Marshal(verdict)
	if err != nil {
		return err
	}
	return validator.ValidateEnvelope("verdict", raw)
}

func containedOutputGrade(grade apiv1.Integrity) bool {
	return grade == "" || grade == apiv1.IntegrityDerived || grade == apiv1.IntegrityUnapproved
}
