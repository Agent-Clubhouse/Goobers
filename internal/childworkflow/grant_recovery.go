package childworkflow

import (
	"context"
	"errors"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// AcquireForContainedPod supports retries of secret delivery for an already
// authenticated exact pod contract. Ordinary local harness acquisition remains
// exclusive: only this trusted exchange may recover the same live credential.
func (r *Runtime) AcquireForContainedPod(ctx context.Context, env apiv1.InvocationEnvelope, secrets interface{ RegisterUntil([]byte, time.Time) }) (*mcpio.ChildWorkflowAccess, func() error, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	issuer := r.issuer
	issuer.Secrets = secrets
	access, closeAccess, err := issuer.Acquire(ctx, env)
	if !errors.Is(err, ErrAuthorityChanged) && !errors.Is(err, triggerqueue.ErrChildAuthorityChanged) {
		return access, closeAccess, err
	}
	return issuer.recoverDelivery(ctx, env)
}

func (s *GrantIssuer) recoverDelivery(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
	authority, err := s.Authority.PrepareStage(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	o := authority.Origin
	binding, err := s.Queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: o.Gaggle, ParentRunID: o.RunID}, o.StageOccurrence)
	if err != nil {
		return nil, nil, err
	}
	if binding.AttemptID != o.AttemptID || binding.ConfigDigest != o.ConfigDigest || binding.PolicyDigest != o.PolicyDigest {
		return nil, nil, ErrAuthorityChanged
	}
	if err = s.Queue.CheckChildAuthority(ctx, binding, s.now()); err != nil {
		return nil, nil, err
	}
	token, err := s.Grants.RecoverChildWorkflowGrant(podauth.ChildWorkflowGrant{ID: binding.GrantID, Gaggle: o.Gaggle, RunID: o.RunID, StageOccurrence: o.StageOccurrence, AttemptID: o.AttemptID, ConfigDigest: o.ConfigDigest, PolicyDigest: o.PolicyDigest, ExpiresAt: binding.ExpiresAt})
	if err != nil {
		return nil, nil, err
	}
	access := &mcpio.ChildWorkflowAccess{Endpoint: s.Endpoint, BearerToken: token}
	if err = access.Validate(o.RunID); err != nil {
		return nil, nil, err
	}
	current, err := s.Authority.PrepareStage(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	if !samePreparedAuthority(authority, current) {
		return nil, nil, ErrAuthorityChanged
	}
	if err = s.Queue.CheckChildAuthority(ctx, binding, s.now()); err != nil {
		return nil, nil, err
	}
	if err = s.Queue.CheckChildParentOpen(ctx, binding.ChildParent); err != nil {
		return nil, nil, err
	}
	s.Secrets.RegisterUntil([]byte(token), binding.ExpiresAt)
	return access, s.revoker(binding), nil
}
