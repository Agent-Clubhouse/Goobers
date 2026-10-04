package childworkflow

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/mcpio"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

// StageAuthorityPreparer derives authority from the actual active journal
// attempt and its immutable configuration, intersected with current permissions.
type StageAuthorityPreparer interface {
	PrepareStage(context.Context, apiv1.InvocationEnvelope) (Authority, error)
}

// GrantIssuer connects harness launch to signed credentials and durable attempt
// ownership. It uses the submission queue and shared secret registry; there is
// no separate in-memory authority map. The daemon must use the same signing key
// in HTTPService and the same registry in its journal scrubbers.
type GrantIssuer struct {
	Queue     *triggerqueue.Store
	Grants    *podauth.SignedKey
	Authority StageAuthorityPreparer
	Endpoint  string
	Secrets   interface{ RegisterUntil([]byte, time.Time) }
	Now       func() time.Time
}

// Acquire implements the harness's trusted access provider. A repeated launch
// of the same live attempt cannot steal its credential. A new journal attempt
// replaces the prior grant by CAS; a still-active attempt may renew after expiry,
// but revocation never permits same-attempt renewal. Logical waits are not bound
// by this token lifetime and require renewal before resumed agent work.
func (s *GrantIssuer) Acquire(ctx context.Context, env apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
	if s == nil || s.Queue == nil || s.Grants == nil || s.Authority == nil || s.Secrets == nil || env.ChildWorkflowOrigin == nil {
		return nil, nil, ErrAuthorityUnavailable
	}
	authority, err := s.Authority.PrepareStage(ctx, env)
	if err != nil {
		return nil, nil, err
	}
	if authority.Origin.GrantID != "" {
		return nil, nil, ErrAuthorityUnavailable
	}
	previous, err := s.previousGrant(ctx, authority.Origin)
	if err != nil {
		return nil, nil, err
	}
	origin := authority.Origin
	token, grant, err := s.Grants.MintChildWorkflowGrant(podauth.ChildWorkflowGrant{
		Gaggle: origin.Gaggle, RunID: origin.RunID, StageOccurrence: origin.StageOccurrence,
		AttemptID: origin.AttemptID, ConfigDigest: origin.ConfigDigest, PolicyDigest: origin.PolicyDigest,
	}, podauth.MaxChildWorkflowGrantTTL)
	if err != nil {
		return nil, nil, err
	}
	access := &mcpio.ChildWorkflowAccess{Endpoint: s.Endpoint, BearerToken: token}
	if err := access.Validate(origin.RunID); err != nil {
		return nil, nil, err
	}
	origin.GrantID = grant.ID
	binding := origin.Binding(grant.ExpiresAt)
	if err := s.Queue.BindChildAuthority(ctx, binding, previous, s.now()); err != nil {
		return nil, nil, err
	}
	closeAccess := s.revoker(binding)
	current, err := s.Authority.PrepareStage(ctx, env)
	if err == nil && !samePreparedAuthority(authority, current) {
		err = ErrAuthorityChanged
	}
	if err == nil {
		err = ctx.Err()
	}
	if err != nil {
		return nil, nil, errors.Join(err, closeAccess())
	}
	s.Secrets.RegisterUntil([]byte(token), grant.ExpiresAt)
	return access, closeAccess, nil
}

func (s *GrantIssuer) previousGrant(ctx context.Context, origin Origin) (string, error) {
	current, err := s.Queue.ChildAuthority(ctx, triggerqueue.ChildParent{Gaggle: origin.Gaggle, ParentRunID: origin.RunID}, origin.StageOccurrence)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	if current.AttemptID == origin.AttemptID && (current.Revoked || current.ExpiresAt.After(s.now())) {
		return "", ErrAuthorityChanged
	}
	return current.GrantID, nil
}

func samePreparedAuthority(a, b Authority) bool {
	return a.Origin == b.Origin && a.Actor == b.Actor && a.ConfigGeneration == b.ConfigGeneration &&
		a.ParentWorkflowDigest == b.ParentWorkflowDigest && a.ParentGooberDigest == b.ParentGooberDigest
}

func (s *GrantIssuer) revoker(binding triggerqueue.ChildAuthority) func() error {
	var once sync.Once
	var result error
	return func() error {
		once.Do(func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			result = s.Queue.RevokeChildAuthority(ctx, binding)
			// An older attempt finishing must not revoke its replacement.
			if errors.Is(result, triggerqueue.ErrChildAuthorityChanged) {
				result = nil
			}
		})
		return result
	}
}

func (s *GrantIssuer) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}
