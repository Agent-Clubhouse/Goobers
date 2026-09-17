package executor

import (
	"fmt"
	"time"

	apiv1 "github.com/goobers/goobers/api/v1alpha1"
)

// TimeoutSource names which configuration surface supplied a stage's effective
// execution deadline (#5265).
//
// The point of naming the source is that the value alone does not tell an
// operator which clock would stop their stage. "10m" could be the built-in
// fallback, a runner default the instance configured, or a value the workflow
// declared — and the fix differs in each case. Reported as the surface's own
// spelling so it can be searched for directly in a workflow or instance.yaml.
type TimeoutSource string

const (
	// TimeoutSourceLimits is limits.maxDurationSeconds on the task.
	TimeoutSourceLimits TimeoutSource = "limits.maxDurationSeconds"
	// TimeoutSourceInput is the legacy inputs.timeout duration string.
	TimeoutSourceInput TimeoutSource = "inputs.timeout"
	// TimeoutSourceRunnerDefault is the executor's configured default, which
	// instance.yaml's stage-timeout setting resolves into.
	TimeoutSourceRunnerDefault TimeoutSource = "runner default"
	// TimeoutSourceBuiltinDefault is the package DefaultTimeout fallback. It is
	// a FALLBACK, not a universal hard cap: it applies only when no surface
	// above it supplied a value.
	TimeoutSourceBuiltinDefault TimeoutSource = "built-in default"
)

// Output keys that publish the resolved deadline on every stage result, so the
// effective value and its origin are visible without reading executor code.
// Mirrors the existing stdoutTruncated/networkIsolation output convention.
const (
	// OutputTimeoutSeconds is the effective deadline in seconds.
	OutputTimeoutSeconds = "timeoutSeconds"
	// OutputTimeoutSource is the TimeoutSource that supplied it.
	OutputTimeoutSource = "timeoutSource"
)

// ResolvedTimeout is one stage's effective execution deadline plus the surface
// it came from.
type ResolvedTimeout struct {
	Duration time.Duration
	Source   TimeoutSource
}

// Immediate reports the deadline that expires the instant execution starts.
//
// This is a real, reachable configuration — a legacy inputs.timeout of "0s"
// parses to zero and is honored as written — and it is NOT the same as either
// of the other two zeros in this area: limits.maxDurationSeconds=0 means unset
// and falls through, while a task timeoutSeconds of 0 is rejected before it
// ever reaches an executor. #5265 requires those three meanings be preserved
// distinctly, so this predicate exists to name the one that survives rather
// than to change it.
func (r ResolvedTimeout) Immediate() bool {
	return r.Duration <= 0
}

// Describe renders the deadline and its source for operator-facing output.
func (r ResolvedTimeout) Describe() string {
	if r.Immediate() {
		return fmt.Sprintf("%s (from %s; expires immediately)", r.Duration, r.Source)
	}
	return fmt.Sprintf("%s (from %s)", r.Duration, r.Source)
}

// resolveTimeout applies the documented precedence for one stage.
//
// Precedence, highest first — unchanged from what timeoutFor already did, with
// the source now carried alongside the value:
//
//  1. limits.maxDurationSeconds, when positive. Zero here means UNSET and falls
//     through; it is not a request for an immediate deadline.
//  2. inputs.timeout, when present and non-empty, parsed as a Go duration. A
//     declared "0s" resolves to an immediately expiring deadline, which is the
//     legacy meaning #5265 requires be preserved.
//  3. The executor's configured default, when positive.
//  4. The package built-in default.
//
// Queue and admission deadlines are deliberately absent: they bound how long a
// stage may WAIT to start, not how long it may run, and #5265 requires the two
// be shown separately rather than conflated into one number.
func (e *ShellExecutor) resolveTimeout(env apiv1.InvocationEnvelope) (ResolvedTimeout, error) {
	if env.Limits.MaxDurationSeconds > 0 {
		return ResolvedTimeout{
			Duration: time.Duration(env.Limits.MaxDurationSeconds) * time.Second,
			Source:   TimeoutSourceLimits,
		}, nil
	}
	if s := stringInput(env, InputTimeout); s != "" {
		d, err := time.ParseDuration(s)
		if err != nil {
			return ResolvedTimeout{}, fmt.Errorf("executor: invalid %s input %q: %w", InputTimeout, s, err)
		}
		return ResolvedTimeout{Duration: d, Source: TimeoutSourceInput}, nil
	}
	if e.DefaultTimeout > 0 {
		return ResolvedTimeout{Duration: e.DefaultTimeout, Source: TimeoutSourceRunnerDefault}, nil
	}
	return ResolvedTimeout{Duration: DefaultTimeout, Source: TimeoutSourceBuiltinDefault}, nil
}

// publishResolvedTimeout records the effective deadline on the stage result.
//
// Published on every outcome, not just a timeout: an operator asking "which
// clock would stop this stage?" needs the answer from a stage that SUCCEEDED,
// which is exactly the case where a timeout error message never appears.
func publishResolvedTimeout(result *apiv1.ResultEnvelope, resolved ResolvedTimeout) {
	if result == nil {
		return
	}
	if result.Outputs == nil {
		result.Outputs = map[string]interface{}{}
	}
	result.Outputs[OutputTimeoutSeconds] = resolved.Duration.Seconds()
	result.Outputs[OutputTimeoutSource] = string(resolved.Source)
}
