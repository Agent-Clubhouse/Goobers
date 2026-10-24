package engine

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"go.temporal.io/sdk/temporal"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/mcpio"
)

type fakeChildAuthority struct {
	acquired   []ChildAuthorityRequest
	released   []ChildAuthorityRelease
	grant      ChildAuthorityGrant
	acquireErr error
	releaseErr []error
}

func (f *fakeChildAuthority) AcquireChildAuthority(_ context.Context, request ChildAuthorityRequest) (ChildAuthorityGrant, error) {
	f.acquired = append(f.acquired, request)
	return f.grant, f.acquireErr
}

func (f *fakeChildAuthority) ReleaseChildAuthority(_ context.Context, release ChildAuthorityRelease) error {
	f.released = append(f.released, release)
	if len(f.releaseErr) == 0 {
		return nil
	}
	err := f.releaseErr[0]
	f.releaseErr = f.releaseErr[1:]
	return err
}

const authorityRunID = "0123456789abcdef0123456789abcdef"

func childCapableInput() DispatchStageInput {
	return DispatchStageInput{Envelope: apiv1.InvocationEnvelope{RunID: authorityRunID,
		ChildWorkflowOrigin: &apiv1.ChildWorkflowOrigin{StageOccurrence: "occ-1", AttemptID: "att-1"}}}
}

func liveGrant() ChildAuthorityGrant {
	return ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "goobers-child.signed", GrantID: "grant-1", ExpiresAt: time.Now().Add(time.Hour)}
}

func TestStageChildAuthorityStampsTheGrantOntoAFreshAttempt(t *testing.T) {
	authority := &fakeChildAuthority{grant: liveGrant()}
	a := &Activities{ChildAuthority: authority}
	var attempt dispatcher.Attempt
	release, err := a.stageChildAuthority(t.Context(), childCapableInput(), &attempt)
	if err != nil || release == nil {
		t.Fatalf("acquire = %v (release %v)", err, release != nil)
	}
	if len(authority.acquired) != 1 || authority.acquired[0].RunID != authorityRunID || authority.acquired[0].Origin.AttemptID != "att-1" {
		t.Fatalf("acquired %+v", authority.acquired)
	}
	var access mcpio.ChildWorkflowAccess
	if err := json.Unmarshal([]byte(attempt.ChildWorkflowAccess), &access); err != nil || access.BearerToken != "goobers-child.signed" || access.Endpoint != "https://control.example" {
		t.Fatalf("attempt carries %q (%v)", attempt.ChildWorkflowAccess, err)
	}
	release(nil)
	if len(authority.released) != 1 || authority.released[0].GrantID != "grant-1" || authority.released[0].Origin.AttemptID != "att-1" {
		t.Fatalf("released %+v, want the exact grant", authority.released)
	}
}

func TestStageChildAuthorityIsOnlyForFreshChildCapableAgenticAttempts(t *testing.T) {
	for name, edit := range map[string]func(*DispatchStageInput){
		"deterministic stage": func(in *DispatchStageInput) { in.Run = &apiv1.DeterministicRun{Command: []string{"true"}} },
		"no child origin":     func(in *DispatchStageInput) { in.Envelope.ChildWorkflowOrigin = nil },
		"adopted pod":         func(in *DispatchStageInput) { in.Custody = &dispatcher.PodCustody{} },
	} {
		t.Run(name, func(t *testing.T) {
			authority := &fakeChildAuthority{grant: liveGrant()}
			input := childCapableInput()
			edit(&input)
			var attempt dispatcher.Attempt
			release, err := (&Activities{ChildAuthority: authority}).stageChildAuthority(t.Context(), input, &attempt)
			if err != nil || release != nil || len(authority.acquired) != 0 || attempt.ChildWorkflowAccess != "" {
				t.Fatalf("release %v, %v, acquired %d, attempt %q", release != nil, err, len(authority.acquired), attempt.ChildWorkflowAccess)
			}
		})
	}
}

func TestStageChildAuthorityFailsClosed(t *testing.T) {
	var attempt dispatcher.Attempt
	_, err := (&Activities{}).stageChildAuthority(t.Context(), childCapableInput(), &attempt)
	var application *temporal.ApplicationError
	if !errors.As(err, &application) || !application.NonRetryable() {
		t.Fatalf("no authority = %v, want a final refusal", err)
	}
	for name, tc := range map[string]struct {
		authority *fakeChildAuthority
		retryable bool
	}{
		"control unavailable": {authority: &fakeChildAuthority{acquireErr: fmt.Errorf("failover: %w", ErrChildCustodyUnavailable)}, retryable: true},
		"control refuses":     {authority: &fakeChildAuthority{acquireErr: fmt.Errorf("superseded: %w", ErrChildCustodyRefused)}},
		"invalid grant":       {authority: &fakeChildAuthority{grant: ChildAuthorityGrant{Endpoint: "https://control.example", BearerToken: "goobers-pod.x", GrantID: "g"}}},
	} {
		t.Run(name, func(t *testing.T) {
			var attempt dispatcher.Attempt
			release, err := (&Activities{ChildAuthority: tc.authority}).stageChildAuthority(t.Context(), childCapableInput(), &attempt)
			if err == nil || release != nil || attempt.ChildWorkflowAccess != "" {
				t.Fatalf("acquire = %v (release %v, attempt %q), want a failure", err, release != nil, attempt.ChildWorkflowAccess)
			}
			var application *temporal.ApplicationError
			final := errors.As(err, &application) && application.NonRetryable()
			if final == tc.retryable {
				t.Fatalf("acquire error %v: final=%v, want retryable=%v", err, final, tc.retryable)
			}
			// A retryable acquire failure is the control plane's, never the
			// stage's: it must not charge the run's stage budget.
			if tc.retryable && (application == nil || application.Type() != FailureTypeInfrastructure || !errors.Is(err, ErrChildCustodyUnavailable)) {
				t.Fatalf("unavailable acquire error = %v, want a %s failure wrapping the unavailability", err, FailureTypeInfrastructure)
			}
		})
	}
}

// The grant is released when the attempt's use of it ends, and kept when a
// Temporal retry of the same attempt, or a pod outliving the activity, still
// needs it.
func TestReleasesChildAuthorityOnlyWhenTheAttemptIsDoneWithIt(t *testing.T) {
	for name, tc := range map[string]struct {
		outcome error
		release bool
	}{
		"settled":           {outcome: nil, release: true},
		"final failure":     {outcome: temporal.NewNonRetryableApplicationError("refused", "x", nil), release: true},
		"cancelled":         {outcome: fmt.Errorf("activity: %w", context.Canceled), release: true},
		"retryable failure": {outcome: temporal.NewApplicationError("infra", FailureTypeInfrastructure)},
		"timeout":           {outcome: context.DeadlineExceeded},
		"worker handoff":    {outcome: fmt.Errorf("drain: %w", dispatcher.ErrWorkerHandoff)},
		// What the activity actually returns on a handoff: the reattach path
		// turns it into a retryable application error that no longer wraps
		// the sentinel. Kept, because the pod outlives the activity.
		"handoff as a retryable application error": {outcome: temporal.NewApplicationError("dispatch handed off to another worker", FailureTypeInfrastructure)},
		"unclassified": {outcome: errors.New("dispatch fault")},
	} {
		t.Run(name, func(t *testing.T) {
			authority := &fakeChildAuthority{grant: liveGrant()}
			var attempt dispatcher.Attempt
			release, err := (&Activities{ChildAuthority: authority}).stageChildAuthority(t.Context(), childCapableInput(), &attempt)
			if err != nil {
				t.Fatal(err)
			}
			release(tc.outcome)
			if released := len(authority.released) == 1; released != tc.release {
				t.Fatalf("released=%v, want %v", released, tc.release)
			}
		})
	}
}

func TestStageChildAuthorityReleaseRetriesAnUnavailableControlPlane(t *testing.T) {
	authority := &fakeChildAuthority{grant: liveGrant(), releaseErr: []error{fmt.Errorf("failover: %w", ErrChildCustodyUnavailable), nil}}
	var attempt dispatcher.Attempt
	release, err := (&Activities{ChildAuthority: authority}).stageChildAuthority(t.Context(), childCapableInput(), &attempt)
	if err != nil {
		t.Fatal(err)
	}
	release(nil)
	if len(authority.released) != 2 {
		t.Fatalf("release calls = %d, want a retry after unavailability", len(authority.released))
	}
}
