package runtimewiring

import (
	"context"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/mcpio"
)

const deliveredRun = "0123456789abcdef0123456789abcdef"

func deliveredEnvelope() apiv1.InvocationEnvelope {
	return apiv1.InvocationEnvelope{RunID: deliveredRun, ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occ", AttemptID: "att"}}
}

func TestDeliveredChildWorkflowAccessServesAndRegistersTheGrant(t *testing.T) {
	t.Setenv(dispatcher.EnvChildWorkflowAccess, `{"endpoint":"https://control.example","bearerToken":"goobers-child.signed-grant"}`)
	registry := journal.NewRegistryScrubber()
	fallbackCalled := false
	fallback := func(context.Context, apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		fallbackCalled = true
		return nil, nil, errors.New("contained-parent exchange")
	}
	access, closeAccess, err := DeliveredChildWorkflowAccess(registry, fallback)(t.Context(), deliveredEnvelope())
	if err != nil || access.Endpoint != "https://control.example" || access.BearerToken != "goobers-child.signed-grant" || closeAccess == nil || fallbackCalled {
		t.Fatalf("access = %v, close %v, %v (fallback %v)", access, closeAccess != nil, err, fallbackCalled)
	}
	if err := closeAccess(); err != nil {
		t.Fatalf("close = %v; the worker releases the grant", err)
	}
	if scrubbed := string(registry.Scrub([]byte("tool said goobers-child.signed-grant"))); strings.Contains(scrubbed, "signed-grant") {
		t.Fatalf("the delivered bearer is not registered for scrubbing: %q", scrubbed)
	}
}

func TestDeliveredChildWorkflowAccessDefersToTheExchangeWithoutAGrant(t *testing.T) {
	fallbackCalled := false
	fallback := func(context.Context, apiv1.InvocationEnvelope) (*mcpio.ChildWorkflowAccess, func() error, error) {
		fallbackCalled = true
		return nil, nil, errors.New("contained-parent exchange")
	}
	if _, _, err := DeliveredChildWorkflowAccess(journal.NewRegistryScrubber(), fallback)(t.Context(), deliveredEnvelope()); err == nil || !fallbackCalled {
		t.Fatalf("without a delivered grant: %v (fallback %v)", err, fallbackCalled)
	}
	if _, _, err := DeliveredChildWorkflowAccess(journal.NewRegistryScrubber(), nil)(t.Context(), deliveredEnvelope()); err == nil {
		t.Fatal("no grant and no exchange produced access")
	}
}

func TestDeliveredChildWorkflowAccessRefusesABadGrantWithoutEchoingIt(t *testing.T) {
	for name, tc := range map[string]struct {
		delivered string
		env       apiv1.InvocationEnvelope
	}{
		"malformed":     {delivered: `{"bearerToken":"goobers-child.secret-value"`, env: deliveredEnvelope()},
		"not a grant":   {delivered: `{"endpoint":"https://control.example","bearerToken":"goobers-pod.secret-value"}`, env: deliveredEnvelope()},
		"endpoint path": {delivered: `{"endpoint":"https://control.example/x","bearerToken":"goobers-child.secret-value"}`, env: deliveredEnvelope()},
		"no origin":     {delivered: `{"endpoint":"https://control.example","bearerToken":"goobers-child.secret-value"}`, env: apiv1.InvocationEnvelope{RunID: deliveredRun}},
	} {
		t.Run(name, func(t *testing.T) {
			t.Setenv(dispatcher.EnvChildWorkflowAccess, tc.delivered)
			access, _, err := DeliveredChildWorkflowAccess(journal.NewRegistryScrubber(), nil)(t.Context(), tc.env)
			if err == nil || access != nil {
				t.Fatalf("bad grant served: %v, %v", access, err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatalf("refusal echoes the bearer: %v", err)
			}
		})
	}
}
