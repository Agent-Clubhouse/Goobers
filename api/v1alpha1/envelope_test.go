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
		Workspace:  "/var/goobers/runs/0af7/worktrees/implement-001",
		Item: &BacklogItem{
			ID: "1421", Provider: ProviderGitHub, Title: "Add rate limiting",
			URL: "https://github.com/acme/web/issues/1421", Labels: []string{"goobers"},
		},
		RepoRef: RepoRef{Provider: ProviderGitHub, Owner: "acme", Name: "web", Branch: "main"},
		ContextPointers: []ContextPointer{
			{Name: "plan", Artifact: &ArtifactPointer{Path: "artifacts/plan/plan.md", Digest: Digest([]byte("plan"))}},
			{Name: "issue", External: &ExternalRef{Kind: "issue", URI: "https://github.com/acme/web/issues/1421"}},
		},
		Capabilities: []string{"repo:push", "github:pr:write"},
		Limits:       Limits{MaxDurationSeconds: 1800, MaxTokens: 2_000_000, MaxCostUSD: 5},
		Inputs:       map[string]interface{}{"draftPr": true},
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
		Status:    ResultFailure,
		Outputs:   map[string]interface{}{"attempts": float64(3)},
		Artifacts: []ArtifactPointer{{Path: "artifacts/impl/log.txt", Digest: Digest([]byte("log")), MediaType: "text/plain", Size: 3}},
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
		Decision:  VerdictNeedsChanges,
		Rationale: "one bug blocks a pass",
		Evidence:  []ArtifactPointer{{Path: "artifacts/review/diff.patch", Digest: Digest([]byte("diff"))}},
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
	for _, s := range []ResultStatus{ResultSuccess, ResultFailure, ResultBlocked} {
		if !s.IsValid() {
			t.Errorf("expected %q to be valid", s)
		}
	}
	if ResultStatus("failed").IsValid() {
		t.Error("expected legacy \"failed\" to be invalid under the V0 contract")
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

// TestInvocationHasNoUpstreamResultField is a structural guard for invariant
// §2.4: the invocation envelope must expose no field carrying an upstream stage's
// result body. If someone adds one, this test fails and forces the conversation.
func TestInvocationHasNoUpstreamResultField(t *testing.T) {
	rt := reflect.TypeOf(InvocationEnvelope{})
	for i := 0; i < rt.NumField(); i++ {
		ft := rt.Field(i).Type
		// Reject any field whose type is (or contains) a ResultEnvelope.
		if typeMentions(ft, reflect.TypeOf(ResultEnvelope{})) {
			t.Fatalf("InvocationEnvelope.%s carries a ResultEnvelope — that is cross-stage state reach-through (§2.4). Stages consume upstream work via ContextPointers only.", rt.Field(i).Name)
		}
	}
}

// TestTypeMentionsRecursesIntoStructFields is the regression test for #125:
// typeMentions previously only matched a direct/pointer/slice/map reference
// to target, so a field of a type that merely WRAPS target in another struct
// evaded the reach-through guard entirely.
func TestTypeMentionsRecursesIntoStructFields(t *testing.T) {
	target := reflect.TypeOf(ResultEnvelope{})

	type wrapper struct{ R ResultEnvelope }
	if !typeMentions(reflect.TypeOf(wrapper{}), target) {
		t.Fatal("typeMentions did not catch a ResultEnvelope nested inside another struct")
	}

	type wrapperPtr struct{ R *ResultEnvelope }
	if !typeMentions(reflect.TypeOf(wrapperPtr{}), target) {
		t.Fatal("typeMentions did not catch a *ResultEnvelope nested inside another struct")
	}

	type unrelated struct{ S string }
	if typeMentions(reflect.TypeOf(unrelated{}), target) {
		t.Fatal("typeMentions false-positived on an unrelated struct")
	}
}

// typeMentions reports whether t is, or (recursively) contains a field of,
// target — the reach-through guard's actual check. It recurses into struct
// fields (#125) so a field whose type merely wraps target in another struct
// (e.g. struct{ R ResultEnvelope }), not just a direct/pointer/slice/map
// reference, still gets caught.
func typeMentions(t, target reflect.Type) bool {
	if t == target {
		return true
	}
	switch t.Kind() {
	case reflect.Pointer, reflect.Slice, reflect.Array, reflect.Chan:
		return typeMentions(t.Elem(), target)
	case reflect.Map:
		return typeMentions(t.Key(), target) || typeMentions(t.Elem(), target)
	case reflect.Struct:
		for i := 0; i < t.NumField(); i++ {
			if typeMentions(t.Field(i).Type, target) {
				return true
			}
		}
		return false
	default:
		return false
	}
}
