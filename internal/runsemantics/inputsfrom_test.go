package runsemantics

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

func TestResolveInputsFrom(t *testing.T) {
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{
		"isBehindBase": true,
		// A legacy output key that literally contains a dot. This is the case
		// a prior attempt at #562 broke, and the reason the qualified reading
		// is conditional rather than unconditional.
		"legacy.dotted.key": "kept",
	}}
	completed := StageOutputs{
		"pr-select": {Outputs: map[string]any{"selectedNumber": 42, "head": "feature"}},
		"review":    {Outputs: map[string]any{"reviewDigest": "sha256:abc"}},
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
			got, ok := ResolveInputsFrom(tc.value, upstream, completed, true)
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
	completed := StageOutputs{"pr-select": {Outputs: map[string]any{"other": "x"}}}

	if _, ok := ResolveInputsFrom("pr-select.selectedNumber", upstream, completed, true); ok {
		t.Fatal("a qualified reference to a known stage must not fall back to the preceding stage's outputs")
	}
}

func TestInputsFromErrorNamesWhatTheStageActuallyEmitted(t *testing.T) {
	completed := StageOutputs{"pr-select": {Outputs: map[string]any{"head": "f", "base": "main"}}}
	err := InputsFromError("gather", "pullNumber", "pr-select.selectedNumber", apiv1.ResultEnvelope{}, completed, true)
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

// #6890: a stage that FAILED and advanced through a gate leaves its consumer
// with a missing input; the error must name the failure that caused it rather
// than read as if the producer forgot to emit.
func TestInputsFromErrorNamesTheUpstreamFailureThatCausedTheMiss(t *testing.T) {
	failed := apiv1.ResultEnvelope{
		Status:  apiv1.ResultFailure,
		Outputs: map[string]any{"integrity": "unapproved", "stderr": "x"},
		Error:   &apiv1.ErrorInfo{Code: "github_server_error", Message: "select pull requests: 503 class_saturated"},
	}
	msg := InputsFromError("gather-pr-context", "selectedNumber", "selectedNumber", failed, StageOutputs{}, false).Error()
	for _, want := range []string{"selectedNumber", "integrity, stderr", "FAILED", "github_server_error", "class_saturated"} {
		if !strings.Contains(msg, want) {
			t.Errorf("error %q should mention %q", msg, want)
		}
	}
	ok := apiv1.ResultEnvelope{Status: apiv1.ResultSuccess, Outputs: map[string]any{"x": "y"}}
	if msg := InputsFromError("gather-pr-context", "selectedNumber", "selectedNumber", ok, StageOutputs{}, false).Error(); strings.Contains(msg, "FAILED") {
		t.Errorf("a successful producer must not be blamed: %q", msg)
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
	completed := StageOutputs{"pr-select": {Outputs: map[string]any{"selectedNumber": 42}}}

	got, ok := ResolveInputsFrom("pr-select.selectedNumber", upstream, completed, false)
	if !ok {
		t.Fatal("the literal key must still resolve when qualified resolution is off")
	}
	if got != "literal" {
		t.Errorf("value = %#v, want the literal bare key; a pre-562 DSL version must not gain qualified resolution", got)
	}

	got, ok = ResolveInputsFrom("pr-select.selectedNumber", upstream, completed, true)
	if !ok || got != 42 {
		t.Errorf("value = %#v (ok=%v), want the qualified value once the feature is enabled", got, ok)
	}
}
