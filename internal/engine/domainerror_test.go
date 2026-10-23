package engine

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	commonpb "go.temporal.io/api/common/v1"
	enumspb "go.temporal.io/api/enums/v1"
	failurepb "go.temporal.io/api/failure/v1"
	"go.temporal.io/sdk/temporal"

	"github.com/goobers/goobers/internal/attemptidentity"
	"github.com/goobers/goobers/internal/dispatcher"
	"github.com/goobers/goobers/internal/invoke"
	"github.com/goobers/goobers/internal/journal"
	"github.com/goobers/goobers/internal/runsemantics"
	"github.com/goobers/goobers/internal/telemetry"
)

func TestDomainFailureSurvivesTemporalTransport(t *testing.T) {
	for _, domain := range []error{
		invoke.InfrastructureFailure(errors.New("provider unavailable")),
		invoke.InfrastructureFailureUntil(fmt.Errorf("connect: %w", errors.New("refused")), time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)),
		invoke.InfrastructureFailure(runsemantics.CodedStageFailure("github_auth_failed", errors.New("denied"))),
		invoke.Timeout(errors.New("expired")),
		errors.Join(errors.New("dispatch failed"), errors.New("cleanup failed")),
	} {
		t.Run(domain.Error(), func(t *testing.T) {
			original := classifySeamError(domain)
			transport := agenticUsageError(withDispatchFailurePlacement(original, dispatcher.Report{Runner: "remote", Pod: "pod"}), AttemptUsage{Reported: true})
			converter := temporal.GetDefaultFailureConverter()
			transport = converter.FailureToError(converter.ErrorToFailure(transport))
			err := converter.FailureToError(&failurepb.Failure{Message: "activity transport", Cause: converter.ErrorToFailure(transport), FailureInfo: &failurepb.Failure_ActivityFailureInfo{ActivityFailureInfo: &failurepb.ActivityFailureInfo{ActivityType: &commonpb.ActivityType{Name: "InvokeGoober"}, RetryState: enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED}}})
			class, classErr := ClassifyDispatchFailure(err)
			if classErr != nil {
				t.Fatal(classErr)
			}
			event := executorErrorEvent("work", 1, "", class, err, nil)
			if want := journal.ErrorDetailFor("executor_error", domain); !reflect.DeepEqual(event.Error, want) {
				t.Fatalf("detail=%+v want=%+v", event.Error, want)
			}
			code, errorClass := runsemantics.ClassifyDispatchError(domain)
			if event.Runner["errorCode"] != code || event.Runner["errorClass"] != string(errorClass) {
				t.Fatalf("classification=%v", event.Runner)
			}
			if DispatchFailurePlacement(err) == nil || !dispatchFailureUsage(err).Reported {
				t.Fatal("other evidence lost")
			}
			var before, after *temporal.ApplicationError
			if !errors.As(original, &before) || !errors.As(err, &after) || before.Type() != after.Type() || before.Message() != after.Message() || before.NonRetryable() != after.NonRetryable() {
				t.Fatal("Temporal failure semantics changed")
			}
			now := time.Date(2025, 12, 31, 23, 0, 0, 0, time.UTC)
			baseline := domainFailureTransport(classifySeamTransportError(domain))
			if got, want := infrastructureRetryDelay(err, time.Second, now), infrastructureRetryDelay(baseline, time.Second, now); got != want {
				t.Fatalf("retry delay=%v want=%v", got, want)
			}
		})
	}
}

func TestDomainFailureNMinusOneKeepsJournalBehavior(t *testing.T) {
	for _, old := range []error{
		temporal.NewApplicationError("legacy", FailureTypeInfrastructure),
		temporal.NewApplicationError("legacy evidence", FailureTypeInfrastructure, time.Time{}, dispatchFailureEvidence{SchemaVersion: 1, Usage: &AttemptUsage{Reported: true}}),
		temporal.NewApplicationError("future evidence", FailureTypeInfrastructure, time.Time{}, dispatchFailureEvidence{SchemaVersion: 2, Domain: &domainFailure{Error: &journal.ErrorDetail{Message: "must not use"}}}),
	} {
		converter := temporal.GetDefaultFailureConverter()
		old = converter.FailureToError(converter.ErrorToFailure(old))
		event := executorErrorEvent("work", 1, "", journal.AttemptInfra, old, nil)
		if !reflect.DeepEqual(event.Error, journal.ErrorDetailFor("executor_error", old)) || event.Runner["errorCode"] != telemetry.ErrCodeInfraFailure {
			t.Fatalf("N-1 behavior changed: %+v", event)
		}
	}
}

type commitObservation struct {
	Workspace
	committed bool
	err       error
}

func (w commitObservation) HasNewCommits(context.Context) (bool, error) { return w.committed, w.err }

func TestDomainFailureRecordsCommittedWorkBeforeTeardown(t *testing.T) {
	for _, committed := range []bool{false, true} {
		domain := invoke.InfrastructureFailure(errors.New("provider failed"))
		err := classifyAgenticFailure(t.Context(), commitObservation{committed: committed}, domain)
		converter := temporal.GetDefaultFailureConverter()
		got := domainFailureFromError(converter.FailureToError(converter.ErrorToFailure(err)))
		if got == nil || got.InfraCommittedWork != committed {
			t.Fatalf("evidence=%+v want committed=%v", got, committed)
		}
	}
	policy := classifyAgenticFailure(t.Context(), commitObservation{committed: true}, errors.New("policy failed"))
	if domainFailureFromError(policy).InfraCommittedWork {
		t.Fatal("policy failure invented infrastructure committed work")
	}
}

func TestDomainFailureCommitInspectionErrorDoesNotAssertWork(t *testing.T) {
	cause := invoke.InfrastructureFailure(errors.New("provider failed"))
	inspection := errors.New("repository unreadable")
	err := classifyAgenticFailure(t.Context(), commitObservation{committed: true, err: inspection}, cause)
	got := domainFailureFromError(err)
	want := journal.ErrorDetailFor("executor_error", errors.Join(cause, fmt.Errorf("inspect commits created by infrastructure-failed attempt: %w", inspection)))
	if got == nil || got.InfraCommittedWork || !reflect.DeepEqual(got.Error, want) {
		t.Fatalf("inspection failure evidence=%+v want=%+v", got, want)
	}
}

func TestDomainFailureDoesNotMasqueradeAsAttemptIdentity(t *testing.T) {
	fresh := classifySeamError(invoke.InfrastructureFailure(errors.New("unavailable")))
	converter := temporal.GetDefaultFailureConverter()
	for _, err := range []error{fresh, converter.FailureToError(converter.ErrorToFailure(fresh))} {
		if identity := attemptIdentityFromError(err); identity != nil {
			t.Fatalf("domain evidence invented identity: %+v", identity)
		}
	}
}

// Match workerhost's trackedActivityInbound: the outer application error
// carries attempt identity, while its cause retains the classified evidence.
func TestDomainFailureSurvivesWorkerhostIdentityWrapper(t *testing.T) {
	domain := invoke.InfrastructureFailure(errors.New("provider unavailable"))
	inner := classifySeamError(domain)
	var app *temporal.ApplicationError
	if !errors.As(inner, &app) {
		t.Fatal("classified error is not an ApplicationError")
	}
	outer := temporal.NewApplicationErrorWithOptions(inner.Error(), app.Type(), temporal.ApplicationErrorOptions{
		NonRetryable: app.NonRetryable(), Cause: inner, NextRetryDelay: app.NextRetryDelay(), Category: app.Category(),
		Details: []interface{}{attemptidentity.Identity{WorkerIdentity: "worker-a", BuildID: "b1"}},
	})
	err := domainFailureTransport(outer)
	if id := attemptIdentityFromError(err); id == nil || id.WorkerIdentity != "worker-a" || id.BuildID != "b1" {
		t.Fatalf("identity wrapper not visible: %+v", id)
	}
	event := executorErrorEvent("work", 1, "", journal.AttemptInfra, err, nil)
	if want := journal.ErrorDetailFor("executor_error", domain); !reflect.DeepEqual(event.Error, want) {
		t.Fatalf("journal recorded wrapper instead of domain: got %+v want %+v", event.Error, want)
	}
	if event.Runner["errorCode"] != telemetry.ErrCodeInfraFailure || event.Runner["errorClass"] != string(telemetry.ErrorClassInfra) {
		t.Fatalf("domain classification lost: %v", event.Runner)
	}
}

func domainFailureTransport(err error) error {
	converter := temporal.GetDefaultFailureConverter()
	return converter.FailureToError(&failurepb.Failure{Message: "activity transport", Cause: converter.ErrorToFailure(err),
		FailureInfo: &failurepb.Failure_ActivityFailureInfo{ActivityFailureInfo: &failurepb.ActivityFailureInfo{
			ActivityType: &commonpb.ActivityType{Name: "InvokeGoober"}, RetryState: enumspb.RETRY_STATE_MAXIMUM_ATTEMPTS_REACHED}}})
}

func TestDomainFailurePreservesRetryAt(t *testing.T) {
	retryAt := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	domain := invoke.InfrastructureFailureUntil(errors.New("rate limited"), retryAt)
	now := retryAt.Add(-time.Hour)
	want := infrastructureRetryDelay(domainFailureTransport(classifySeamTransportError(domain)), time.Second, now)
	if want != time.Hour {
		t.Fatalf("pre-envelope delay=%v want one hour", want)
	}
	if got := infrastructureRetryDelay(domainFailureTransport(classifySeamError(domain)), time.Second, now); got != want {
		t.Fatalf("enveloped retry delay=%v want %v", got, want)
	}
}
