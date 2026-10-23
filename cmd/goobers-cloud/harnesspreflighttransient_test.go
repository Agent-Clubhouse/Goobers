package main

import (
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/localscheduler"
)

func TestWorkerTransientHarnessRefusalCarriesRetryTime(t *testing.T) {
	copilotOnly := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "copilot-only"}
	bothHarnesses := localscheduler.WorkflowIdentity{Gaggle: "example", Workflow: "both"}
	retryAt := time.Date(2026, 10, 14, 12, 5, 0, 0, time.UTC)
	seams := &gaggleSeams{
		harnessRefusals: map[localscheduler.WorkflowIdentity]string{
			copilotOnly:   `stage "write" requires harness "copilot": API rate limit exceeded`,
			bothHarnesses: `stage "review" requires harness "claude-code": not logged in`,
		},
		harnessRetryAt: map[localscheduler.WorkflowIdentity]time.Time{copilotOnly: retryAt},
	}
	err := seams.refuseUnavailableHarness(apiv1.InvocationEnvelope{Gaggle: "example", WorkflowID: "copilot-only"})
	if got, ok := invoke.InfrastructureRetryAt(err); !invoke.IsInfrastructureFailure(err) || !ok || !got.Equal(retryAt) {
		t.Fatalf("transient refusal = %v (retryAt %s, %t), want an infrastructure failure retrying at %s", err, got, ok, retryAt)
	}
	err = seams.refuseUnavailableHarness(apiv1.InvocationEnvelope{Gaggle: "example", WorkflowID: "both"})
	if _, ok := invoke.InfrastructureRetryAt(err); !invoke.IsInfrastructureFailure(err) || ok {
		t.Fatalf("permanent refusal = %v, want a plain infrastructure failure", err)
	}
}
