package v1alpha1

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestInvocationEnvelopeRoundTrip(t *testing.T) {
	in := InvocationEnvelope{
		TaskID:     "implement-001",
		WorkflowID: "default-implement",
		RunID:      "0af7651916cd43dd8448eb211c80319c",
		Gaggle:     "acme-web",
		Goal:       "Implement the backlog item and open a PR.",
		Item: &BacklogItem{
			ID: "1421", Provider: ProviderGitHub, Title: "Add rate limiting",
			URL: "https://github.com/acme/web/issues/1421", Labels: []string{"goobers"},
		},
		RepoRef: RepoRef{Provider: ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		Limits:  Limits{MaxDurationSeconds: 1800, MaxTokens: 2_000_000, MaxCostUSD: 5},
		Inputs:  map[string]interface{}{"draftPr": true},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out InvocationEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in: %#v\nout: %#v", in, out)
	}
}

func TestResultEnvelopeRoundTrip(t *testing.T) {
	in := ResultEnvelope{
		Status:    ResultFailed,
		Outputs:   map[string]interface{}{"attempts": float64(3)},
		Artifacts: []Artifact{{Type: "log", URI: "https://logs/run/1", Label: "run log"}},
		Summary:   "could not satisfy the failing test",
		Metrics:   map[string]float64{"durationSeconds": 12.5},
		Error:     &ErrorInfo{Code: "test_failure", Message: "TestFoo failed", Retryable: true},
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out ResultEnvelope
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in: %#v\nout: %#v", in, out)
	}
}

func TestVerdictRoundTrip(t *testing.T) {
	in := Verdict{
		Decision: VerdictNeedsChanges,
		Findings: []Finding{
			{Severity: SeverityError, Message: "not concurrency safe", Location: "x.go:10"},
			{Severity: SeverityWarning, Message: "missing test"},
		},
		Summary: "one bug, one gap",
	}
	data, err := json.Marshal(in)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var out Verdict
	if err := json.Unmarshal(data, &out); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !reflect.DeepEqual(in, out) {
		t.Errorf("round-trip mismatch:\n in: %#v\nout: %#v", in, out)
	}
}

func TestStatusAndDecisionValidity(t *testing.T) {
	for _, s := range []ResultStatus{ResultSuccess, ResultFailed, ResultNeedsEscalation} {
		if !s.IsValid() {
			t.Errorf("expected %q to be valid", s)
		}
	}
	if ResultStatus("done").IsValid() {
		t.Error("expected \"done\" to be invalid")
	}
	for _, d := range []VerdictDecision{VerdictPass, VerdictFail, VerdictNeedsChanges} {
		if !d.IsValid() {
			t.Errorf("expected %q to be valid", d)
		}
	}
	if VerdictDecision("approved").IsValid() {
		t.Error("expected \"approved\" to be invalid")
	}
}
