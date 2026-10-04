package main

import (
	"context"

	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
)

func (s *daemonCredentialService) applyContainedCredentialCeiling(ctx context.Context, pinned pinnedStage, request httpapi.CredentialResolveRequest) (context.Context, *childCredentialLease, error) {
	p, ok := httpapi.PrincipalFromContext(ctx)
	if !ok || !p.WorkflowParent {
		return s.applyChildCredentialCeiling(ctx, pinned)
	}
	a, err := s.parentAttempt(ctx)
	if err != nil || a.contract.Identity.RunID != request.RunID || a.contract.Stage != request.Stage || !sameParentCredentialIdentity(a, pinned) || s.children == nil {
		return nil, nil, parentAuthorityRefusal()
	}
	env, err := a.envelope(ctx)
	if err != nil {
		return nil, nil, parentAuthorityRefusal()
	}
	held, err := s.children.AcquirePreparedStage(ctx, env)
	if err != nil {
		return nil, nil, parentAuthorityRefusal()
	}
	ceiling := credentials.NewChildCeiling(false, held.Authority.Admission.ParentTask.Capabilities, held.Authority.Admission.ParentTask.Capabilities)
	if !sameChildCeiling(ceiling, a.contract.Ceiling) {
		held.Release()
		return nil, nil, parentAuthorityRefusal()
	}
	bound, err := credentials.WithChildCeiling(ctx, ceiling)
	if err != nil {
		held.Release()
		return nil, nil, err
	}
	return bound, &childCredentialLease{ceiling: ceiling, release: held.Release, verify: held.Verify}, nil
}

func sameParentCredentialIdentity(a parentAttemptCustody, p pinnedStage) bool {
	id := a.contract.Identity
	return id.RunID == p.identity.RunID && id.ConfigGeneration == p.identity.ConfigGeneration && id.WorkflowDigest == p.identity.WorkflowDigest && id.GooberDigest == p.identity.GooberDigest && !p.profile.deterministic
}
