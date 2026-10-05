package main

import (
	"context"
	"net/http"
	"slices"

	"github.com/goobers/goobers/internal/childworkflow"
	"github.com/goobers/goobers/internal/credentials"
	"github.com/goobers/goobers/internal/httpapi"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runner"
)

type childCredentialLease struct {
	ceiling credentials.ChildCeiling
	release func()
	verify  func(context.Context) error
}

func (l *childCredentialLease) finish(ctx context.Context) error {
	if l.verify == nil {
		return nil
	}
	if err := l.verify(ctx); err != nil {
		return credentialPlaneError(http.StatusForbidden, "child_credentials_revoked", "child delegation was revoked before credentials could be returned")
	}
	return nil
}

func (s *daemonCredentialService) applyChildCredentialCeiling(ctx context.Context, pinned pinnedStage, requestedStage ...string) (context.Context, *childCredentialLease, error) {
	var attempt *childAttemptCustody
	if p, ok := httpapi.PrincipalFromContext(ctx); ok && p.GeneratedChild {
		a, err := s.childAttempt(ctx)
		if err != nil || len(requestedStage) != 1 || a.contract.Identity.RunID != pinned.identity.RunID || a.contract.Stage != requestedStage[0] || a.active(ctx) != nil {
			return nil, nil, credentialPlaneError(http.StatusForbidden, "child_attempt_unavailable", "child credential request differs from active signed contract")
		}
		attempt = &a
	}
	if pinned.identity.Child == nil {
		return ctx, &childCredentialLease{release: func() {}}, nil
	}
	refuse := func() (context.Context, *childCredentialLease, error) {
		return nil, nil, credentialPlaneError(http.StatusForbidden, "child_credentials_unavailable", "child source, current delegation or credential custody could not be verified")
	}
	if s.childCredentials == nil {
		return refuse()
	}
	dir, err := s.layout.FindRunDir(pinned.identity.RunID)
	if err != nil {
		return refuse()
	}
	reader, err := journal.OpenReadOnly(dir)
	if err != nil {
		return refuse()
	}
	stored, err := runner.PinnedChildCredentials(reader, pinned.identity)
	if err != nil || stored == nil {
		return refuse()
	}
	lease, err := s.childCredentials(ctx, pinned.identity)
	if err != nil {
		return refuse()
	}
	if lease == nil || lease.release == nil || lease.verify == nil {
		return refuse()
	}
	if !sameChildCeiling(*stored, lease.ceiling) {
		lease.release()
		return refuse()
	}
	effective := lease.ceiling
	if attempt != nil {
		effective = lease.ceiling.ModelOnly()
		if !sameChildCeiling(effective, attempt.contract.Ceiling) {
			lease.release()
			return refuse()
		}
	}
	childCtx, err := credentials.WithChildCeiling(ctx, effective)
	if err != nil {
		lease.release()
		return refuse()
	}
	if attempt != nil {
		verify := lease.verify
		lease.verify = func(ctx context.Context) error {
			if err := attempt.active(ctx); err != nil {
				return err
			}
			return verify(ctx)
		}
	}
	return childCtx, lease, nil
}

func sameChildCeiling(a, b credentials.ChildCeiling) bool {
	return a.Version == b.Version && a.AllowPublication == b.AllowPublication && slices.Equal(a.AllowedKeys, b.AllowedKeys)
}

// credentialCeiling independently resolves exact accepted provenance. Its live
// authority lease stays held until the caller finishes token materialization;
// a config reload cannot publish revocation in the middle of issuance.
func (l *queuedChildLauncher) credentialCeiling(ctx context.Context, id journal.RunIdentity) (*childCredentialLease, error) {
	ref, err := l.retainedChildIdentity(ctx, id)
	if err != nil {
		return nil, err
	}
	a, release, err := l.acquire(ctx, ref.Envelope)
	if err != nil {
		return nil, err
	}
	source, err := l.queue.ChildProposal(ctx, ref.Child.Identity)
	if err != nil {
		release()
		return nil, err
	}
	proposal, err := childworkflow.ValidateRetainedStart(a, ref.Envelope, source.Source)
	if err != nil {
		release()
		return nil, err
	}
	return &childCredentialLease{ceiling: proposal.CredentialCeiling(), release: release, verify: func(ctx context.Context) error { _, err := l.retainedChildIdentity(ctx, id); return err }}, nil
}

func (l *queuedChildLauncher) retainedChildIdentity(ctx context.Context, id journal.RunIdentity) (childExecutionRef, error) {
	return retainedChildExecutionRef(ctx, l.queue, id, true)
}
