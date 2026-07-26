package runner

import (
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
	"github.com/goobers/goobers/internal/journal"
)

func TestResolveInputsFrom(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{
		"isBehindBase": true,
		// A legacy output key that literally contains a dot. This is the case
		// a prior attempt at #562 broke, and the reason the qualified reading
		// is conditional rather than unconditional.
		"legacy.dotted.key": "kept",
	}}
	completed := stageOutputs{
		"pr-select": {"selectedNumber": 42, "head": "feature"},
		"review":    {"reviewDigest": "sha256:abc"},
	}

	for _, tc := range []struct {
		name  string
		value string
		want  any
		found bool
	}{
		{
			name:  "bare key still reads the immediately preceding stage",
			value: "isBehindBase",
			want:  true,
			found: true,
		},
		{
			name:  "qualified reference reaches past the preceding stage",
			value: "pr-select.selectedNumber",
			want:  42,
			found: true,
		},
		{
			name:  "qualified reference to a different upstream stage",
			value: "review.reviewDigest",
			want:  "sha256:abc",
			found: true,
		},
		{
			// The whole point of the conditional rule: "legacy" is not a stage,
			// so the entire string stays a bare key.
			name:  "legacy dotted key is not mistaken for a qualified reference",
			value: "legacy.dotted.key",
			want:  "kept",
			found: true,
		},
		{
			name:  "qualified reference to an unknown key on a real stage",
			value: "pr-select.nope",
			found: false,
		},
		{
			name:  "bare key absent upstream",
			value: "missing",
			found: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveInputsFrom(tc.value, upstream, completed, true)
			if ok != tc.found {
				t.Fatalf("resolved = %v, want found=%v", ok, tc.found)
			}
			if tc.found && got != tc.want {
				t.Errorf("value = %#v, want %#v", got, tc.want)
			}
		})
	}
}

// A qualified reference must never silently fall back to the preceding stage
// when the named stage ran but lacks the key — that would reintroduce the
// silent-wrong-value class the feature exists to remove.
func TestResolveInputsFromDoesNotFallBackWithinAKnownStage(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{"selectedNumber": "wrong"}}
	completed := stageOutputs{"pr-select": {"other": "x"}}

	if _, ok := resolveInputsFrom("pr-select.selectedNumber", upstream, completed, true); ok {
		t.Fatal("a qualified reference to a known stage must not fall back to the preceding stage's outputs")
	}
}

func TestInputsFromErrorNamesWhatTheStageActuallyEmitted(t *testing.T) {
	completed := stageOutputs{"pr-select": {"head": "f", "base": "main"}}
	err := inputsFromError("gather", "pullNumber", "pr-select.selectedNumber", completed, true)
	if err == nil {
		t.Fatal("want an error")
	}
	msg := err.Error()
	for _, want := range []string{"pr-select", "selectedNumber", "base, head"} {
		if !contains(msg, want) {
			t.Errorf("error %q should mention %q", msg, want)
		}
	}
}

// Resume must rebuild the same map the live walk accumulated, or a qualified
// reference resolves before a crash and fails after one.
func TestReconstructStageOutputsFromJournal(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageStarted, Stage: "pr-select"},
		{Type: journal.EventStageFinished, Stage: "pr-select", Outputs: map[string]any{"selectedNumber": 7}},
		{Type: journal.EventStageFinished, Stage: "gather", Outputs: map[string]any{"files": 3}},
	}
	got := reconstructStageOutputs(events)
	if got["pr-select"]["selectedNumber"] != 7 {
		t.Errorf("pr-select outputs = %#v, want selectedNumber 7", got["pr-select"])
	}
	if got["gather"]["files"] != 3 {
		t.Errorf("gather outputs = %#v, want files 3", got["gather"])
	}
}

// A repassed stage's later attempt supersedes its earlier one, matching what
// the live walk does when it re-records the stage.
func TestReconstructStageOutputsLastAttemptWins(t *testing.T) {
	events := []journal.Event{
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 1, Outputs: map[string]any{"digest": "old"}},
		{Type: journal.EventStageFinished, Stage: "implement", Attempt: 2, Outputs: map[string]any{"digest": "new"}},
	}
	if got := reconstructStageOutputs(events)["implement"]["digest"]; got != "new" {
		t.Errorf("digest = %v, want the later attempt's value", got)
	}
}

func TestReconstructStageOutputsIsNilWhenNothingFinished(t *testing.T) {
	if got := reconstructStageOutputs([]journal.Event{{Type: journal.EventRunStarted}}); got != nil {
		t.Errorf("stage outputs = %#v, want nil", got)
	}
}

func contains(haystack, needle string) bool {
	return len(needle) == 0 || (len(haystack) >= len(needle) && indexOf(haystack, needle) >= 0)
}

func indexOf(haystack, needle string) int {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return i
		}
	}
	return -1
}

// Under a DSL version that predates #562, a dotted value is ALWAYS a bare key —
// even when its prefix names a stage that ran. Without this gate, adding the
// feature would change what an already-released version means underneath
// workflows authored against it.
func TestQualifiedResolutionIsGatedByDSLVersion(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{"pr-select.selectedNumber": "literal"}}
	completed := stageOutputs{"pr-select": {"selectedNumber": 42}}

	got, ok := resolveInputsFrom("pr-select.selectedNumber", upstream, completed, false)
	if !ok {
		t.Fatal("the literal key must still resolve when qualified resolution is off")
	}
	if got != "literal" {
		t.Errorf("value = %#v, want the literal bare key; a pre-562 DSL version must not gain qualified resolution", got)
	}

	got, ok = resolveInputsFrom("pr-select.selectedNumber", upstream, completed, true)
	if !ok || got != 42 {
		t.Errorf("value = %#v (ok=%v), want the qualified value once the feature is enabled", got, ok)
	}
}

// Fan-in: a join reads a specific branch's stage output through the
// four-segment form (§7).
func TestResolveBranchQualifiedFanIn(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{"unrelated": 1}}
	completed := stageOutputs{
		"review-security": {"findingsRef": "sha256:sec"},
		"review-perf":     {"findingsRef": "sha256:perf"},
	}

	got, ok := resolveInputsFrom("focus-areas.security.review-security.findingsRef", upstream, completed, true)
	if !ok || got != "sha256:sec" {
		t.Errorf("security fan-in = %#v (ok=%v), want sha256:sec", got, ok)
	}
	got, ok = resolveInputsFrom("focus-areas.performance.review-perf.findingsRef", upstream, completed, true)
	if !ok || got != "sha256:perf" {
		t.Errorf("perf fan-in = %#v (ok=%v), want sha256:perf", got, ok)
	}
}

// A branch that failed, was cancelled, or settled empty produced no stage
// outputs, so its reference resolves to absent — which the join must handle via
// the completeness record rather than receiving a wrong value.
func TestBranchQualifiedReferenceToAFailedBranchIsAbsent(t *testing.T) {
	completed := stageOutputs{"review-security": {"findingsRef": "sha256:sec"}}
	if _, ok := resolveInputsFrom("focus-areas.performance.review-perf.findingsRef",
		apiv1.ResultEnvelope{}, completed, true); ok {
		t.Fatal("a reference into a branch that produced nothing must resolve to absent")
	}
	err := inputsFromError("collate", "perfFindings", "focus-areas.performance.review-perf.findingsRef", completed, true)
	for _, want := range []string{"performance", "focus-areas", "review-perf", "completeness record"} {
		if !contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}
}

func TestSplitBranchQualified(t *testing.T) {
	for _, tc := range []struct {
		value                        string
		wantOK                       bool
		parallel, branch, stage, key string
	}{
		{"fan.security.review.findingsRef", true, "fan", "security", "review", "findingsRef"},
		// The output key may itself contain dots; it is the remainder.
		{"fan.security.review.a.b", true, "fan", "security", "review", "a.b"},
		{"only.three.segments", false, "", "", "", ""},
		{"two.segments", false, "", "", "", ""},
		{"nodots", false, "", "", "", ""},
	} {
		p, b, s, k, ok := splitBranchQualified(tc.value)
		if ok != tc.wantOK {
			t.Errorf("%q: ok = %v, want %v", tc.value, ok, tc.wantOK)
			continue
		}
		if ok && (p != tc.parallel || b != tc.branch || s != tc.stage || k != tc.key) {
			t.Errorf("%q split to (%q,%q,%q,%q), want (%q,%q,%q,%q)",
				tc.value, p, b, s, k, tc.parallel, tc.branch, tc.stage, tc.key)
		}
	}
}
