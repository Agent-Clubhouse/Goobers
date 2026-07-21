package runner

import (
	"context"
	"fmt"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

type guardedAssetGoober struct {
	guardActive *bool
}

func (*guardedAssetGoober) HasAssetBundle() bool {
	return true
}

func (g *guardedAssetGoober) Invoke(context.Context, apiv1.InvocationEnvelope) (apiv1.ResultEnvelope, error) {
	if !*g.guardActive {
		return apiv1.ResultEnvelope{}, fmt.Errorf("asset path guard was not active before invocation")
	}
	return apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}, nil
}

func (g *guardedAssetGoober) Review(context.Context, apiv1.InvocationEnvelope) (apiv1.Verdict, error) {
	if !*g.guardActive {
		return apiv1.Verdict{}, fmt.Errorf("asset path guard was not active before review")
	}
	return apiv1.Verdict{Decision: apiv1.VerdictPass}, nil
}

func TestGooberInvocationActivatesAssetPathGuardBeforeCall(t *testing.T) {
	for _, tc := range []struct {
		name string
		call func(*gooberInvocation) error
	}{
		{
			name: "invoke",
			call: func(g *gooberInvocation) error {
				_, err := g.Invoke(context.Background(), apiv1.InvocationEnvelope{})
				return err
			},
		},
		{
			name: "review",
			call: func(g *gooberInvocation) error {
				_, err := g.Review(context.Background(), apiv1.InvocationEnvelope{})
				return err
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			guardActive := false
			invocation := &gooberInvocation{
				Goober: &guardedAssetGoober{guardActive: &guardActive},
				activateAssetPathGuard: func() error {
					guardActive = true
					return nil
				},
			}

			if err := tc.call(invocation); err != nil {
				t.Fatalf("%s: %v", tc.name, err)
			}
			if !invocation.materializedAssets() {
				t.Fatalf("%s did not record materialized assets", tc.name)
			}
		})
	}
}

type gateHeartbeatJournalStub struct{}

func (gateHeartbeatJournalStub) Append(journal.Event) error  { return nil }
func (gateHeartbeatJournalStub) RepairAppendBoundary() error { return nil }

func TestGateHeartbeatGooberComposesWithAssetGuard(t *testing.T) {
	guardActive := false
	invocation := &gooberInvocation{
		Goober: &guardedAssetGoober{guardActive: &guardActive},
		activateAssetPathGuard: func() error {
			guardActive = true
			return nil
		},
	}
	ticker := &fakeHeartbeatTicker{
		ticks:   make(chan time.Time),
		stopped: make(chan struct{}),
	}
	r := &Runner{
		heartbeatInterval:  StageHeartbeatInterval,
		newHeartbeatTicker: func(time.Duration) heartbeatTicker { return ticker },
	}
	goober := gateHeartbeatGoober{
		goober: invocation, runner: r, journal: gateHeartbeatJournalStub{},
		stage: "review", attempt: 1,
	}

	verdict, err := goober.Review(context.Background(), apiv1.InvocationEnvelope{})
	if err != nil {
		t.Fatalf("Review: %v", err)
	}
	if verdict.Decision != apiv1.VerdictPass {
		t.Fatalf("decision = %q, want pass", verdict.Decision)
	}
	if !invocation.materializedAssets() {
		t.Fatal("asset wrapper was not invoked through the heartbeat wrapper")
	}
	select {
	case <-ticker.stopped:
	default:
		t.Fatal("heartbeat ticker was not stopped after review")
	}
}
