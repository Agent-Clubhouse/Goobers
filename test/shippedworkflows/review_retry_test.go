package shippedworkflows

import (
	"path/filepath"
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/instance"
)

// TestShippedImplementationReviewGateDeclaresBoundedRetry is #5397's
// shipped-workflow assertion, loaded through the production config loader
// (internal/workflow's TestShippedImplementationReviewGateRetriesInfrastructureFailure
// asserts the same on the raw YAML). The implementation `review` gate runs
// after a successful implement turn; without an `agentic.retry` a transient
// reviewer-harness infrastructure failure on its only attempt fails the run
// and discards that turn. Evaluator retries are bounded and never charge a
// repass (#765), and a non-zero backoff keeps the retry out of the same
// failure window (#5084).
func TestShippedImplementationReviewGateDeclaresBoundedRetry(t *testing.T) {
	root := repositoryRoot(t)
	checked := 0
	for _, dir := range []string{"reference-workflows", "config-examples"} {
		set, report, err := instance.LoadConfigDir(filepath.Join(root, dir))
		if err != nil {
			t.Fatalf("load %s: %v\n%v", dir, err, report)
		}
		for _, definition := range set.Workflows {
			if !strings.Contains(definition.Name, "implementation") {
				continue
			}
			key := dir + ":" + shippedWorkflowKey(definition.Spec.Gaggle, definition.Name)
			review := requireGate(t, definition.Spec, "review")
			if review.Agentic == nil {
				t.Errorf("%s: review gate is not agentic", key)
				continue
			}
			checked++
			retry := review.Agentic.Retry
			switch {
			case retry == nil:
				t.Errorf("%s: review gate declares no agentic.retry (#5397)", key)
			case retry.MaxAttempts < 2 || retry.MaxAttempts > 5:
				t.Errorf("%s: review gate retry.maxAttempts = %d, want a bounded retry in [2,5]", key, retry.MaxAttempts)
			case retry.BackoffSeconds <= 0:
				t.Errorf("%s: review gate retry.backoffSeconds = %d, want > 0 (#5084)", key, retry.BackoffSeconds)
			}
		}
	}
	// Loud precondition: a renamed or moved workflow must not turn this into
	// a vacuous pass.
	if checked < 8 {
		t.Fatalf("checked %d shipped implementation review gates, want >= 8", checked)
	}
}
