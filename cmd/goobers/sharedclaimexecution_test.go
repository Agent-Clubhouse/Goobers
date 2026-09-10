package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/claimsclient"
	"github.com/goobers/goobers/internal/localscheduler"
	"github.com/goobers/goobers/internal/sharedclaim"
)

type awaitingClaimFenceExecutor struct{ called bool }

func (e *awaitingClaimFenceExecutor) Run(ctx context.Context, _ apiv1.InvocationEnvelope, _ apiv1.DeterministicRun) (apiv1.ResultEnvelope, error) {
	e.called = true
	<-ctx.Done()
	// An executor that drops cancellation must still not report success.
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (e *awaitingClaimFenceExecutor) Invoke(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	return e.Run(ctx, env, apiv1.DeterministicRun{})
}

func (e *awaitingClaimFenceExecutor) Review(ctx context.Context, env apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	_, err := e.Invoke(ctx, env)
	return apiv1.Verdict{}, err
}

func (*awaitingClaimFenceExecutor) HasAssetBundle() bool { return true }

func TestLocalSharedExecutionUsesPersistedLeaseAndPreservesAssetGuard(t *testing.T) {
	for _, kind := range []string{"deterministic", "agentic", "review", "expired"} {
		t.Run(kind, func(t *testing.T) {
			layout, _ := newPinnedClaimResolverRun(t, "shared")
			now := time.Now()
			if kind == "expired" {
				now = now.Add(-time.Hour)
			}
			ledger, err := localscheduler.OpenClaimLedger(filepath.Join(layout.SchedulerDir(), claimLedgerFileName), localscheduler.WithLedgerClock(func() time.Time { return now }))
			if err != nil {
				t.Fatal(err)
			}
			key := localscheduler.ClaimKey{Gaggle: "example", Provider: "github", ExternalID: "42"}
			owner := sharedclaim.Owner{Instance: "instance", Run: "shared-run", Token: "incarnation"}
			if ok, _, err := ledger.ClaimScopedUntil(key, owner.Run, "claim", now.Add(time.Second), owner); err != nil || !ok {
				t.Fatalf("seed shared lease: %v %v", ok, err)
			}
			inner := &awaitingClaimFenceExecutor{}
			start := localSharedExecutionFence(layout)
			goober := claimFencedGoober{Goober: inner, start: start}
			if !goober.HasAssetBundle() {
				t.Fatal("lease wrapper disabled asset protection")
			}
			env := apiv1.InvocationEnvelope{RunID: owner.Run, Gaggle: key.Gaggle, WorkflowID: "claim"}
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			switch kind {
			case "agentic":
				_, err = goober.Invoke(ctx, env)
			case "review":
				_, err = goober.Review(ctx, env)
			default:
				det := claimFencedDeterministic{Deterministic: inner, start: start}
				_, err = det.Run(ctx, env, apiv1.DeterministicRun{})
			}
			if !errors.Is(err, claimsclient.ErrSharedExecutionExpired) {
				t.Fatalf("execution did not propagate lease loss: %v", err)
			}
			if inner.called != (kind != "expired") {
				t.Fatalf("executor called=%v for %s", inner.called, kind)
			}
		})
	}
}

func TestLocalExecutionFenceHonorsLocalPinAndRunIdentity(t *testing.T) {
	layout, _ := newPinnedClaimResolverRun(t, "local")
	env := apiv1.InvocationEnvelope{RunID: "shared-run", Gaggle: "example", WorkflowID: "claim"}
	ctx, stop, err := localSharedExecutionFence(layout)(t.Context(), env)
	defer stop()
	if err != nil || ctx != t.Context() {
		t.Fatalf("local pin unexpectedly requires a lease: %v", err)
	}
	env.WorkflowID = "forged"
	_, stop, err = localSharedExecutionFence(layout)(t.Context(), env)
	defer stop()
	if err == nil {
		t.Fatal("executor accepted a forged workflow identity")
	}
}
