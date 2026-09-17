package executor

import (
	"strings"
	"testing"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TestResolveTimeoutPrecedenceAndSource is #5265's resolution contract, pinned
// surface by surface: the effective deadline AND the surface that supplied it.
//
// The source is the part that was missing. The ten-minute value is a FALLBACK,
// not a universal hard cap, but a stage that reported only "10m" gave an
// operator no way to tell the built-in fallback from a runner default or a
// value their own workflow declared — which is the difference between where
// they go to change it.
//
// The three zero meanings #5265 requires be preserved distinctly are all
// covered below: limits.maxDurationSeconds=0 is UNSET and falls through, a
// legacy inputs.timeout of "0s" expires immediately, and a task timeoutSeconds
// of 0 is rejected before it reaches an executor (so it cannot appear here at
// all — see the note on that case).
func TestResolveTimeoutPrecedenceAndSource(t *testing.T) {
	const runnerDefault = 25 * time.Minute

	for _, tc := range []struct {
		name           string
		defaultTimeout time.Duration
		limits         apiv1.Limits
		inputs         map[string]interface{}
		want           time.Duration
		wantSource     TimeoutSource
		wantImmediate  bool
	}{
		{
			// Nothing declared anywhere: the built-in fallback, named as such.
			name: "omitted everywhere falls back to the built-in default",
			want: DefaultTimeout, wantSource: TimeoutSourceBuiltinDefault,
		},
		{
			name:           "runner default outranks the built-in default",
			defaultTimeout: runnerDefault,
			want:           runnerDefault, wantSource: TimeoutSourceRunnerDefault,
		},
		{
			name:   "positive limits wins outright",
			limits: apiv1.Limits{MaxDurationSeconds: 90},
			want:   90 * time.Second, wantSource: TimeoutSourceLimits,
		},
		{
			name:   "positive inputs.timeout is used when limits is unset",
			inputs: map[string]interface{}{InputTimeout: "45s"},
			want:   45 * time.Second, wantSource: TimeoutSourceInput,
		},
		{
			// Conflicting settings: limits outranks both the legacy input and
			// the runner default, and the reported source says which won.
			name:           "conflicting surfaces resolve to limits",
			defaultTimeout: runnerDefault,
			limits:         apiv1.Limits{MaxDurationSeconds: 90},
			inputs:         map[string]interface{}{InputTimeout: "45s"},
			want:           90 * time.Second, wantSource: TimeoutSourceLimits,
		},
		{
			name:           "conflicting input and runner default resolve to the input",
			defaultTimeout: runnerDefault,
			inputs:         map[string]interface{}{InputTimeout: "45s"},
			want:           45 * time.Second, wantSource: TimeoutSourceInput,
		},
		{
			// ZERO MEANING 1: limits.maxDurationSeconds=0 is UNSET. It must
			// fall through, never request an immediate deadline.
			name:           "zero limits means unset and falls through",
			defaultTimeout: runnerDefault,
			limits:         apiv1.Limits{MaxDurationSeconds: 0},
			want:           runnerDefault, wantSource: TimeoutSourceRunnerDefault,
		},
		{
			name:           "zero limits falls through to a declared input, not to the default",
			defaultTimeout: runnerDefault,
			limits:         apiv1.Limits{MaxDurationSeconds: 0},
			inputs:         map[string]interface{}{InputTimeout: "45s"},
			want:           45 * time.Second, wantSource: TimeoutSourceInput,
		},
		{
			// ZERO MEANING 2: a legacy inputs.timeout of "0s" expires
			// immediately. Honored as written — #5265 preserves this.
			name:           "legacy zero input expires immediately",
			defaultTimeout: runnerDefault,
			inputs:         map[string]interface{}{InputTimeout: "0s"},
			want:           0, wantSource: TimeoutSourceInput, wantImmediate: true,
		},
		{
			// An empty string is ABSENT, not zero: it falls through. This is
			// the distinction that keeps an unset value threaded through a
			// workflow from silently becoming an instant deadline.
			name:           "empty input string is absent and falls through",
			defaultTimeout: runnerDefault,
			inputs:         map[string]interface{}{InputTimeout: ""},
			want:           runnerDefault, wantSource: TimeoutSourceRunnerDefault,
		},
		{
			name:           "negative limits is not positive and falls through",
			defaultTimeout: runnerDefault,
			limits:         apiv1.Limits{MaxDurationSeconds: -1},
			want:           runnerDefault, wantSource: TimeoutSourceRunnerDefault,
		},
		{
			// A negative legacy duration is already past, so it behaves as an
			// immediate deadline rather than as an absent one.
			name:   "negative input duration is immediate",
			inputs: map[string]interface{}{InputTimeout: "-5s"},
			want:   -5 * time.Second, wantSource: TimeoutSourceInput, wantImmediate: true,
		},
		{
			name:           "negative runner default is not positive and falls through",
			defaultTimeout: -time.Minute,
			want:           DefaultTimeout, wantSource: TimeoutSourceBuiltinDefault,
		},
		{
			name:   "non-string input value is ignored rather than misparsed",
			inputs: map[string]interface{}{InputTimeout: 45},
			want:   DefaultTimeout, wantSource: TimeoutSourceBuiltinDefault,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			e := &ShellExecutor{DefaultTimeout: tc.defaultTimeout}
			env := apiv1.InvocationEnvelope{Limits: tc.limits, Inputs: tc.inputs}

			resolved, err := e.resolveTimeout(env)
			if err != nil {
				t.Fatalf("resolveTimeout: %v", err)
			}
			if resolved.Duration != tc.want {
				t.Errorf("duration = %v, want %v", resolved.Duration, tc.want)
			}
			if resolved.Source != tc.wantSource {
				t.Errorf("source = %q, want %q", resolved.Source, tc.wantSource)
			}
			if resolved.Immediate() != tc.wantImmediate {
				t.Errorf("Immediate() = %v, want %v", resolved.Immediate(), tc.wantImmediate)
			}
			// The bare-value helper must never disagree with the resolution it
			// delegates to: a reported source describing a different value than
			// the one enforced would be worse than reporting no source at all.
			bare, err := e.timeoutFor(env)
			if err != nil {
				t.Fatalf("timeoutFor: %v", err)
			}
			if bare != resolved.Duration {
				t.Errorf("timeoutFor = %v, disagrees with resolveTimeout %v", bare, resolved.Duration)
			}
		})
	}
}

// TestResolveTimeoutRejectsUnparseableInput pins that an unparseable legacy
// duration still fails closed rather than silently falling back to a default —
// an author who wrote a malformed timeout gets told, instead of quietly
// receiving ten minutes.
func TestResolveTimeoutRejectsUnparseableInput(t *testing.T) {
	e := &ShellExecutor{}
	_, err := e.resolveTimeout(apiv1.InvocationEnvelope{
		Inputs: map[string]interface{}{InputTimeout: "ten minutes"},
	})
	if err == nil {
		t.Fatal("resolveTimeout accepted an unparseable duration")
	}
	if !strings.Contains(err.Error(), InputTimeout) {
		t.Errorf("error = %v, want it to name the offending input", err)
	}
}

// TestResolvedTimeoutDescribeNamesSourceAndImmediacy pins the operator-facing
// rendering used in both timeout diagnostics.
func TestResolvedTimeoutDescribeNamesSourceAndImmediacy(t *testing.T) {
	normal := ResolvedTimeout{Duration: 10 * time.Minute, Source: TimeoutSourceBuiltinDefault}
	got := normal.Describe()
	if !strings.Contains(got, "10m") || !strings.Contains(got, string(TimeoutSourceBuiltinDefault)) {
		t.Errorf("Describe() = %q, want the value and its source", got)
	}
	if strings.Contains(got, "immediately") {
		t.Errorf("Describe() = %q, should not claim immediacy for a positive deadline", got)
	}

	// The immediate case is called out explicitly, because "exceeded timeout
	// 0s" reads like a bug report when it is in fact the configured behavior.
	immediate := ResolvedTimeout{Source: TimeoutSourceInput}
	if got := immediate.Describe(); !strings.Contains(got, "immediately") {
		t.Errorf("Describe() = %q, want the immediate case named", got)
	}
}

// TestPublishResolvedTimeoutIsVisibleOnEveryOutcome pins that the effective
// deadline reaches the stage result's Outputs, which is the surface a run
// inspector and a downstream stage both read. #5265 asks that operators be able
// to see which clock would stop execution; a value only present on failure
// would not answer that for a stage that succeeded.
func TestPublishResolvedTimeoutIsVisibleOnEveryOutcome(t *testing.T) {
	var result apiv1.ResultEnvelope
	publishResolvedTimeout(&result, ResolvedTimeout{
		Duration: 90 * time.Second, Source: TimeoutSourceLimits,
	})
	if got, ok := result.Outputs[OutputTimeoutSeconds].(float64); !ok || got != 90 {
		t.Errorf("outputs[%s] = %v, want 90", OutputTimeoutSeconds, result.Outputs[OutputTimeoutSeconds])
	}
	if got, ok := result.Outputs[OutputTimeoutSource].(string); !ok || got != string(TimeoutSourceLimits) {
		t.Errorf("outputs[%s] = %v, want %q", OutputTimeoutSource,
			result.Outputs[OutputTimeoutSource], TimeoutSourceLimits)
	}
	// A nil result must not panic: publication is reporting, and reporting must
	// never be the thing that fails a stage.
	publishResolvedTimeout(nil, ResolvedTimeout{})
}
