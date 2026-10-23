package baseline

import (
	"strings"
	"testing"

	"github.com/goobers/goobers/internal/executor"
)

func TestMixedRunnerFailureSignatureKeepsGoTimeout(t *testing.T) {
	const passing = "ok 159 [fixture-chromium] > e2e/run-stage-inspector.spec.ts:121:1 > handles Error: unavailable (3.8s)\n"
	const failure = "panic: test timed out after 40m0s"
	const failedOutput = failure + "\nFAIL\tgithub.com/goobers/goobers/test/shippedworkflows\t2401.213s\n"
	const trailer = "ci: shipped-workflows: exit status 1\nmake: *** [ci] Error 1\n"

	diagnostic := executor.FailureDiagnostic([]byte(passing+failedOutput), []byte(trailer))
	stageText := "command exited 1; failure: " + diagnostic + "; 2 distinct failure line(s) recorded in failureDigest"
	want := failureSignature(failedOutput)
	for name, signature := range map[string]string{
		"stage":          failureSignature(stageText),
		"combined probe": probeSignature(ProbeResult{Output: passing + failedOutput, Stderr: trailer}),
		"split probe":    probeSignature(ProbeResult{Output: passing, Stderr: failedOutput + trailer}),
	} {
		if signature != want || !strings.Contains(signature, "panic: test timed out") || strings.Contains(signature, "e2e/") {
			t.Errorf("%s signature = %q, want Go timeout signature %q", name, signature, want)
		}
	}
	digest, count := executor.FailureDigest([]byte(passing+failedOutput), []byte(trailer))
	if count != 2 || strings.Contains(digest, "e2e/") {
		t.Fatalf("failure roster contains a passing test or omits the timeout: count %d, digest %q", count, digest)
	}
	if _, complete := failureRoster(digest, count); !complete {
		t.Fatalf("mixed-runner failure roster is incomplete: %q", digest)
	}
}
