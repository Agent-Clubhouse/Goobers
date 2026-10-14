package main

import (
	"context"
	"errors"
	"reflect"
	"strings"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/blobstore"
	"github.com/goobers/goobers/internal/childpod"
	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
)

type parentAttemptCustody struct {
	contract childpod.Contract
	digest   string
	blobs    childpod.ParentAttemptBlobs
	reader   *journal.Reader
}

// The authenticated contract selects one physical parent attempt. Host-retained
// identity and custody are checked independently of request body fields.
func (s *daemonCredentialService) parentAttempt(ctx context.Context) (parentAttemptCustody, error) {
	p, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || p.Issuer != httpapi.WorkflowParentPrincipalIssuer || p.WorkflowParent == nil || !blobstore.ValidDigest(p.WorkflowParent.ContractDigest) {
		return parentAttemptCustody{}, childworkflow.ErrAuthorityUnavailable
	}
	runID, ok := strings.CutPrefix(p.Subject, "run:")
	if !ok || !apiv1.ValidRunID(runID) {
		return parentAttemptCustody{}, childworkflow.ErrAuthorityUnavailable
	}
	dir, err := s.layout.FindRunDir(runID)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	id, err := reader.Identity()
	if err != nil || id.RunID != runID || id.Child != nil {
		return parentAttemptCustody{}, errors.Join(childworkflow.ErrAuthorityUnavailable, err)
	}
	blobs := childpod.ParentAttemptBlobs{Store: childpod.ParentBlobs{RunDir: dir, Identity: id}, ContractDigest: p.WorkflowParent.ContractDigest}
	raw, err := blobs.Get(ctx, p.WorkflowParent.ContractDigest)
	if err != nil {
		return parentAttemptCustody{}, err
	}
	c, err := childpod.DecodeContract(raw, p.WorkflowParent.ContractDigest)
	if err != nil || c.ParentOrigin == nil || !reflect.DeepEqual(c.Identity, id) {
		return parentAttemptCustody{}, errors.Join(childworkflow.ErrAuthorityUnavailable, err)
	}
	return parentAttemptCustody{contract: c, digest: p.WorkflowParent.ContractDigest, blobs: blobs, reader: reader}, nil
}

func (a parentAttemptCustody) active(ctx context.Context) error {
	_, started, err := childworkflow.VerifyActiveStage(ctx, a.reader, a.contract.Identity.RunID, *a.contract.ParentOrigin)
	if err != nil {
		return err
	}
	if started.Seq != uint64(a.contract.PodAttempt) || started.Branch != a.contract.ParentBranch || started.Attempt != a.contract.Attempt || !started.Time.Equal(a.contract.StartedAt) {
		return childworkflow.ErrAuthorityChanged
	}
	return nil
}

// Late output is permitted only until the exact physical writer is joined.
// This check alone never grants credentials or execution authority.
func (a parentAttemptCustody) custody(ctx context.Context) error {
	pending, err := childpod.ParentCustodyPending(ctx, a.reader, a.digest)
	if err != nil {
		return err
	}
	if !pending {
		return childworkflow.ErrAuthorityUnavailable
	}
	return nil
}
