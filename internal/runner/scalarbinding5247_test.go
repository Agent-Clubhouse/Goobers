package runner

import (
	"strings"
	"testing"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestScalarBindingProducerIntermediateConsumer exercises the generic
// producer/intermediate/consumer shape #5247 asks be documented and covered.
//
// Scalar support is not missing — it is the existing mechanism for passing
// small control values between stages — so this pins the behavior that already
// works, generically rather than through one workflow's stage names, and keeps
// the documented rule and the code from drifting apart.
func TestScalarBindingProducerIntermediateConsumer(t *testing.T) {
	// `plan` ran first, then `build`; `build` is the immediately preceding
	// stage, which is what a bare key resolves against.
	completed := stageOutputs{
		"plan":  {outputs: map[string]any{"prTitle": "Add scalar binding docs", "attempt": float64(2)}},
		"build": {outputs: map[string]any{"sha": "abc123", "a.b": "legacy dotted value"}},
	}
	upstream := apiv1.ResultEnvelope{Outputs: completed["build"].outputs}

	for _, tc := range []struct {
		name  string
		value string
		want  any
		found bool
	}{
		{
			name:  "bare key resolves against the immediately preceding stage",
			value: "sha", want: "abc123", found: true,
		},
		{
			name:  "stage-qualified key reaches back past the preceding stage",
			value: "plan.prTitle", want: "Add scalar binding docs", found: true,
		},
		{
			name:  "stage-qualified key binds a non-string scalar",
			value: "plan.attempt", want: float64(2), found: true,
		},
		{
			// The fallthrough that makes the whole rule safe: "a" is not a
			// stage, so the ENTIRE string is a bare key, and an output literally
			// named "a.b" keeps working.
			name:  "legacy dotted key still resolves as a whole bare key",
			value: "a.b", want: "legacy dotted value", found: true,
		},
		{
			// A qualified reference to a stage that DID run must not silently
			// fall back to the preceding stage's outputs.
			name:  "qualified reference to a known stage does not fall back",
			value: "plan.sha", found: false,
		},
		{
			name:  "unknown bare key is not found",
			value: "nosuch", found: false,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := resolveInputsFrom(tc.value, upstream, completed, true)
			if ok != tc.found {
				t.Fatalf("resolveInputsFrom(%q) found = %v, want %v", tc.value, ok, tc.found)
			}
			if tc.found && got != tc.want {
				t.Errorf("resolveInputsFrom(%q) = %#v, want %#v", tc.value, got, tc.want)
			}
		})
	}
}

// TestScalarBindingUnqualifiedVersionTreatsEveryValueAsBare pins the
// compatibility half: under a workflow version without stage-qualified inputs,
// a dotted value is a bare key and nothing reaches back to an earlier stage.
func TestScalarBindingUnqualifiedVersionTreatsEveryValueAsBare(t *testing.T) {
	completed := stageOutputs{
		"plan":  {outputs: map[string]any{"prTitle": "unreachable"}},
		"build": {outputs: map[string]any{"a.b": "legacy dotted value"}},
	}
	upstream := apiv1.ResultEnvelope{Outputs: completed["build"].outputs}

	if _, ok := resolveInputsFrom("plan.prTitle", upstream, completed, false); ok {
		t.Error("a pre-qualified-inputs version must not resolve a stage-qualified reference")
	}
	got, ok := resolveInputsFrom("a.b", upstream, completed, false)
	if !ok || got != "legacy dotted value" {
		t.Errorf("legacy dotted key = %#v, %v; want it to keep resolving as a bare key", got, ok)
	}
}

// TestInputsFromDiagnosticsNameKeysNotValues is #5247's diagnostics
// requirement: an author must be able to understand a missing or invalid
// binding WITHOUT the message exposing entire upstream results.
//
// The leak assertion is the load-bearing one. These messages land in run
// journals, PR comments and logs, so a diagnostic that helpfully printed the
// values it could not match would publish upstream result content to all three.
func TestInputsFromDiagnosticsNameKeysNotValues(t *testing.T) {
	const secretish = "s3cr3t-value-that-must-not-leak"
	completed := stageOutputs{
		"build": {outputs: map[string]any{"sha": "abc123", "token": secretish}},
	}
	upstream := apiv1.ResultEnvelope{Outputs: map[string]any{"tag": "v1", "password": secretish}}

	for _, tc := range []struct {
		name     string
		value    string
		contains []string
	}{
		{
			// A stage that RAN but did not emit the key names what it did emit.
			name:  "qualified miss names the stage and its keys",
			value: "build.digest",
			contains: []string{
				`stage "build" produced no output "digest"`,
				"sha, token",
			},
		},
		{
			// The confusing case: the prefix is not a stage that ran, so the
			// whole value became a bare key. The message must state that rule,
			// not just report "not found".
			name:  "dotted prefix that is not a stage explains the bare-key fallthrough",
			value: "nosuchstage.digest",
			contains: []string{
				`no stage "nosuchstage" has produced outputs in this run`,
				"was treated as a single output key",
				"password, tag",
			},
		},
		{
			name:  "bare miss names the preceding stage's keys",
			value: "nosuch",
			contains: []string{
				`upstream output "nosuch" not found`,
				"password, tag",
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			msg := inputsFromError("consumer", "someInput", tc.value, upstream, completed, true).Error()
			for _, want := range tc.contains {
				if !strings.Contains(msg, want) {
					t.Errorf("message %q does not contain %q", msg, want)
				}
			}
			if strings.Contains(msg, secretish) {
				t.Errorf("diagnostic leaked an upstream VALUE: %q", msg)
			}
		})
	}
}

// TestInputsFromDiagnosticsHandleAnEmptyUpstream pins the degenerate case: a
// preceding stage that emitted nothing produces a readable message rather than
// a dangling "emitted: " with nothing after it.
func TestInputsFromDiagnosticsHandleAnEmptyUpstream(t *testing.T) {
	msg := inputsFromError("consumer", "someInput", "missing", apiv1.ResultEnvelope{}, stageOutputs{}, true).Error()
	if !strings.Contains(msg, "nothing") {
		t.Errorf("message %q should say the preceding stage emitted nothing", msg)
	}
}
