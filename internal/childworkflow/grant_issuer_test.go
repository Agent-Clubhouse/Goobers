package childworkflow

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/podauth"
	"github.com/goobers/goobers/internal/triggerqueue"
)

type testStagePreparer func(context.Context, apiv1.InvocationEnvelope) (Authority, error)

func (f testStagePreparer) PrepareStage(ctx context.Context, env apiv1.InvocationEnvelope) (Authority, error) {
	return f(ctx, env)
}

func issuerFixture(t *testing.T) (*GrantIssuer, *SubmissionService, *submissionAuthority, apiv1.InvocationEnvelope) {
	t.Helper()
	s, resolver, _ := submissionFixture(t)
	resolver.current.Origin.GrantID = ""
	resolver.current.Origin.StageOccurrence = "launch-occurrence"
	key, err := podauth.NewSignedKey(bytes.Repeat([]byte("k"), 32))
	if err != nil {
		t.Fatal(err)
	}
	issuer := &GrantIssuer{
		Queue: s.Queue, Grants: key.WithClock(s.Now), Now: s.Now,
		Endpoint: "http://localhost:8080", Secrets: journal.NewRegistryScrubber(),
		Authority: testStagePreparer(func(context.Context, apiv1.InvocationEnvelope) (Authority, error) {
			current := resolver.current
			current.Origin.GrantID = ""
			return current, nil
		}),
	}
	env := apiv1.InvocationEnvelope{RunID: resolver.current.Origin.RunID, ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{
		StageOccurrence: resolver.current.Origin.StageOccurrence, AttemptID: resolver.current.Origin.AttemptID,
	}}
	return issuer, s, resolver, env
}

func TestGrantIssuerSubmissionAndAttemptCleanup(t *testing.T) {
	i, s, resolver, env := issuerFixture(t)
	access, closeAccess, err := i.Acquire(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	grant, err := i.Grants.VerifyChildWorkflowGrant(access.BearerToken)
	if err != nil {
		t.Fatal(err)
	}
	resolver.current.Origin.GrantID = grant.ID
	if bytes.Contains(i.Secrets.(*journal.RegistryScrubber).Scrub([]byte(access.BearerToken)), []byte(access.BearerToken)) {
		t.Fatal("issued credential not registered before exposure")
	}
	accepted, err := s.Submit(t.Context(), resolver.current.Origin, SubmissionRequest{InvocationKey: "one", Source: []byte(validProposal)})
	if err != nil || accepted.Child.RunID == "" {
		t.Fatalf("signed launch binding did not admit submission: %v", err)
	}
	if _, _, err := i.Acquire(t.Context(), env); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("same live attempt reissued: %v", err)
	}
	if err := closeAccess(); err != nil {
		t.Fatal(err)
	}
	if err := closeAccess(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(t.Context(), resolver.current.Origin, "one"); !errors.Is(err, ErrAuthorityUnavailable) {
		t.Fatalf("closed harness retained access: %v", err)
	}
	if _, _, err := i.Acquire(t.Context(), env); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("revoked attempt reissued: %v", err)
	}
}

func TestGrantIssuerReplacementSurvivesOldCleanup(t *testing.T) {
	i, _, resolver, env := issuerFixture(t)
	_, oldClose, err := i.Acquire(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	resolver.current.Origin.AttemptID = "replacement-attempt"
	env.ChildWorkflowOrigin.AttemptID = "replacement-attempt"
	access, closeAccess, err := i.Acquire(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = closeAccess() })
	if err := oldClose(); err != nil {
		t.Fatal(err)
	}
	grant, err := i.Grants.VerifyChildWorkflowGrant(access.BearerToken)
	if err != nil {
		t.Fatal(err)
	}
	origin := resolver.current.Origin
	origin.GrantID = grant.ID
	if err := i.Queue.CheckChildAuthority(t.Context(), origin.Binding(grant.ExpiresAt), i.now()); err != nil {
		t.Fatalf("old cleanup revoked replacement: %v", err)
	}
}

func TestGrantIssuerFencesChangedOrCancelledAuthority(t *testing.T) {
	for _, cancelContext := range []bool{false, true} {
		i, _, resolver, env := issuerFixture(t)
		ctx, cancel := context.WithCancel(t.Context())
		calls := 0
		i.Authority = testStagePreparer(func(context.Context, apiv1.InvocationEnvelope) (Authority, error) {
			calls++
			if calls == 2 {
				if cancelContext {
					cancel()
				} else {
					resolver.current.Origin.PolicyDigest = digest([]byte("changed policy"))
				}
			}
			return resolver.current, nil
		})
		access, _, err := i.Acquire(ctx, env)
		cancel()
		if err == nil || access != nil {
			t.Fatal("changed/cancelled authority exposed credential")
		}
		row, err := i.Queue.ChildAuthority(t.Context(), triggerqueue.ChildParent{Gaggle: resolver.current.Origin.Gaggle, ParentRunID: env.RunID}, env.ChildWorkflowOrigin.StageOccurrence)
		if err != nil || !row.Revoked {
			t.Fatalf("failed launch left active custody: %v", err)
		}
	}
}

func TestGrantIssuerExpiryCanRenewButNotRevocation(t *testing.T) {
	i, _, resolver, env := issuerFixture(t)
	_, oldClose, err := i.Acquire(t.Context(), env)
	if err != nil {
		t.Fatal(err)
	}
	now := i.now().Add(25 * time.Hour)
	i.Now = func() time.Time { return now }
	i.Grants = i.Grants.WithClock(i.Now)
	access, closeAccess, err := i.Acquire(t.Context(), env)
	if err != nil || access == nil {
		t.Fatalf("expired active attempt failed renewal: %v", err)
	}
	if err := oldClose(); err != nil {
		t.Fatal(err)
	}
	grant, _ := i.Grants.VerifyChildWorkflowGrant(access.BearerToken)
	origin := resolver.current.Origin
	origin.GrantID = grant.ID
	if err := i.Queue.CheckChildAuthority(t.Context(), origin.Binding(grant.ExpiresAt), now); err != nil {
		t.Fatal(err)
	}
	if err := closeAccess(); err != nil {
		t.Fatal(err)
	}
	if _, _, err := i.Acquire(t.Context(), env); !errors.Is(err, ErrAuthorityChanged) {
		t.Fatalf("revoked renewal allowed: %v", err)
	}
}
