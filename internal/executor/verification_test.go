package executor

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/creditgraph"
	"github.com/goobers/goobers/internal/journal"
)

type verificationJournal struct {
	events []journal.Event
	err    error
}

func (j *verificationJournal) RecordArtifact(_ string, _ []byte) (journal.Ref, error) {
	return journal.Ref{}, nil
}

func (j *verificationJournal) Append(event journal.Event) error {
	if j.err != nil {
		return j.err
	}
	j.events = append(j.events, event)
	return nil
}

func executorVerificationReport() creditgraph.VerificationReport {
	digest := "sha256:" + strings.Repeat("a", 64)
	return creditgraph.VerificationReport{
		Schema: creditgraph.VerificationSchemaVersion, Property: "roundtrip",
		Subject: digest, Specification: digest, Context: digest,
		Checker:     creditgraph.VerificationChecker{Name: "checker", Version: "1", Digest: digest},
		Assumptions: []string{"bounded inputs"}, Outcome: creditgraph.VerificationProven, Evidence: []string{digest},
	}
}

func TestVerificationOptInAndPublication(t *testing.T) {
	jr := &verificationJournal{}
	executor := ShellExecutor{Journal: jr}
	for _, test := range []struct {
		inputs map[string]any
		want   bool
		err    bool
	}{
		{inputs: nil},
		{inputs: map[string]any{InputVerificationReport: false}},
		{inputs: map[string]any{InputVerificationReport: true}, want: true},
		{inputs: map[string]any{InputVerificationReport: "true"}, want: true},
		{inputs: map[string]any{InputVerificationReport: "false"}},
		{inputs: map[string]any{InputVerificationReport: "yes"}, err: true},
		{inputs: map[string]any{InputVerificationReport: map[string]any{}}, err: true},
	} {
		got, err := executor.verificationEnabled(apiv1.InvocationEnvelope{Inputs: test.inputs})
		if got != test.want || (err != nil) != test.err {
			t.Fatalf("opt-in %v = %v, %v", test.inputs, got, err)
		}
		artifactOnly := ShellExecutor{Journal: struct{ ArtifactRecorder }{jr}}
		if _, err := artifactOnly.verificationEnabled(apiv1.InvocationEnvelope{Inputs: map[string]any{InputVerificationReport: "true"}}); err == nil {
			t.Fatal("enabled verification without an event appender")
		}
	}
	for _, outcome := range []creditgraph.VerificationOutcome{
		creditgraph.VerificationProven, creditgraph.VerificationCounterexample, creditgraph.VerificationInconclusive,
	} {
		report := executorVerificationReport()
		report.Outcome = outcome
		if outcome == creditgraph.VerificationInconclusive {
			report.Reason = "resource limit"
		}
		data, _ := json.Marshal(report)
		result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
		var err error
		executor.publishVerificationReport(apiv1.InvocationEnvelope{TaskID: "check", Attempt: 2}, data, false, journal.Ref{Digest: report.Subject}, &result, &err)
		if err != nil || result.Status != apiv1.ResultSuccess {
			t.Fatalf("publish %s = %+v, %v", outcome, result, err)
		}
		event := jr.events[len(jr.events)-1]
		if event.Stage != "check" || event.Attempt != 2 || event.Runner["executor"] != "deterministic" || event.Runner["kind"] != creditgraph.VerificationAnnotation {
			t.Fatalf("runner provenance lost: %+v", event)
		}
	}
}

func TestVerificationNeverPromotesFailedOrPartialCheckerOutput(t *testing.T) {
	report := executorVerificationReport()
	data, _ := json.Marshal(report)
	for _, test := range []struct {
		name      string
		status    apiv1.ResultStatus
		retErr    error
		data      []byte
		truncated bool
	}{
		{name: "nonzero", status: apiv1.ResultFailure, data: data},
		{name: "executor error", status: apiv1.ResultSuccess, data: data, retErr: errors.New("checker crashed")},
		{name: "truncated", status: apiv1.ResultSuccess, data: data, truncated: true},
		{name: "malformed", status: apiv1.ResultSuccess, data: []byte("not JSON")},
		{name: "too large", status: apiv1.ResultSuccess, data: append(data, []byte(strings.Repeat(" ", creditgraph.MaxVerificationBytes))...)},
	} {
		t.Run(test.name, func(t *testing.T) {
			jr := &verificationJournal{}
			executor := ShellExecutor{Journal: jr}
			result := apiv1.ResultEnvelope{Status: test.status}
			err := test.retErr
			executor.publishVerificationReport(apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1}, test.data, test.truncated, journal.Ref{}, &result, &err)
			if len(jr.events) != 0 || (err == nil && result.Status != apiv1.ResultFailure) {
				t.Fatalf("failed checker output promoted: %+v, %v, %+v", result, err, jr.events)
			}
		})
	}
	jr := &verificationJournal{err: errors.New("journal unavailable")}
	executor := ShellExecutor{Journal: jr}
	result := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess}
	var err error
	executor.publishVerificationReport(apiv1.InvocationEnvelope{TaskID: "check", Attempt: 1}, data, false, journal.Ref{}, &result, &err)
	if !errors.Is(err, jr.err) {
		t.Fatalf("journal failure was hidden: %v", err)
	}
}
