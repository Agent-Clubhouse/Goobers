package baseline

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// stageResultText is the shape a failing local-ci stage carries into a
// classification: the executor's already-extracted diagnostic, wrapped in its
// "command exited N; failure: ..." message and repeated as the summary.
const stageResultText = "command exited 2; failure: --- FAIL: TestAgentInstructions (0.01s) | " +
	"Expected: 3 sections | Received: 2 sections; warnings: separate stderr evidence at bytes 10-40\n" +
	"command exited 2; failure: --- FAIL: TestAgentInstructions (0.01s) | " +
	"Expected: 3 sections | Received: 2 sections; warnings: separate stderr evidence at bytes 10-40"

// probeTranscript is what the SAME failure looks like to a baseline probe: raw
// combined output, complete with the build chatter and the runner's trailers
// that never reach a stage result.
const probeTranscript = `go build ./...
make[1]: Entering directory '/probe/acme-web'
npx vitest run
 ✓ tests/unit/config.test.ts (4 tests) 12ms
--- FAIL: TestAgentInstructions (0.01s)
    Expected: 3 sections
    Received: 2 sections
FAIL
make[1]: Leaving directory '/probe/acme-web'
`

// TestSharedFailureMatchesAcrossStageAndProbeShapes is the regression for the
// asymmetry that would have made #2971 inert in production: the run side and
// the probe side observe the same failure through structurally different text,
// so unless both are reduced to the same diagnostic the fingerprints differ and
// every affected branch is blamed for the base's failure.
func TestSharedFailureMatchesAcrossStageAndProbeShapes(t *testing.T) {
	if FailureSignatureText(stageResultText) != FailureSignatureText(probeTranscript) {
		t.Fatalf("stage text reduced to %q, probe text reduced to %q; want the same diagnostic",
			FailureSignatureText(stageResultText), FailureSignatureText(probeTranscript))
	}

	prober := &stubProber{result: ProbeResult{Output: probeTranscript}}
	e := newEvaluator(t, prober)
	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: stageResultText, RunID: "run-1", Waiter: "101",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassSharedBaselineFailure {
		t.Fatalf("class = %q, want %q: the base fails this command exactly as the branch does",
			decision.Class, ClassSharedBaselineFailure)
	}
	if !decision.Park {
		t.Fatal("a shared baseline failure must park the run by default")
	}
}

// TestDifferentFailuresStillSeparateGuards the other direction: reducing both
// sides to a diagnostic must not collapse genuinely different failures into one
// fingerprint, which would park a branch for a defect it did introduce.
func TestDifferentFailuresStillSeparate(t *testing.T) {
	prober := &stubProber{result: ProbeResult{Output: probeTranscript}}
	e := newEvaluator(t, prober)
	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: "command exited 1; failure: --- FAIL: TestCheckout (0.30s) | checkout_test.go:88: unexpected error",
		RunID:       "run-2", Waiter: "202",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassPRIntroduced {
		t.Fatalf("class = %q, want %q for a failure the base does not reproduce", decision.Class, ClassPRIntroduced)
	}
}

func TestFailureSignatureTextKeepsUnrecognizedEvidence(t *testing.T) {
	const opaque = "the build tool exploded"
	if got := FailureSignatureText("  " + opaque + "\n"); got != opaque {
		t.Fatalf("FailureSignatureText = %q, want the trimmed original when no diagnostic is recognizable", got)
	}
	if got := FailureSignatureText("   "); got != "" {
		t.Fatalf("FailureSignatureText = %q, want empty for blank evidence", got)
	}
}

func TestFailureSignatureTextDropsTheWarningTrailer(t *testing.T) {
	got := FailureSignatureText("command exited 2; failure: make: *** [ci] Error 1; warnings: separate stdout evidence at bytes 1-2")
	if strings.Contains(got, "warnings") {
		t.Fatalf("FailureSignatureText = %q, want the run-local warning byte offsets dropped", got)
	}
}

// blockingProber blocks until released, so a second concurrent Classify has to
// wait on the first rather than starting its own measurement.
type blockingProber struct {
	release chan struct{}
	mu      sync.Mutex
	calls   int
}

func (p *blockingProber) Probe(context.Context, ProbeTarget, []string) (ProbeResult, error) {
	p.mu.Lock()
	p.calls++
	p.mu.Unlock()
	<-p.release
	return ProbeResult{Output: probeTranscript}, nil
}

// TestConcurrentClassifyMeasuresTheBaselineOnce is the collision guard: two
// runs that hit the same red base at the same moment must not each run a full
// CI probe (nor, sharing a probe checkout, tear it out from under each other).
func TestConcurrentClassifyMeasuresTheBaselineOnce(t *testing.T) {
	prober := &blockingProber{release: make(chan struct{})}
	e := newEvaluator(t, prober)
	req := Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: stageResultText,
	}

	var wg sync.WaitGroup
	classes := make([]Class, 2)
	for i := range classes {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			decision, err := e.Classify(context.Background(), req)
			if err != nil {
				t.Errorf("Classify: %v", err)
				return
			}
			classes[index] = decision.Class
		}(i)
	}
	close(prober.release)
	wg.Wait()

	prober.mu.Lock()
	calls := prober.calls
	prober.mu.Unlock()
	if calls != 1 {
		t.Fatalf("probe calls = %d, want 1: concurrent runs share one baseline measurement", calls)
	}
	for _, class := range classes {
		if class != ClassSharedBaselineFailure {
			t.Fatalf("class = %q, want every concurrent run to see %q", class, ClassSharedBaselineFailure)
		}
	}
}

func TestReleaseReadyUnparksSubjectsWhenTheBaseAdvances(t *testing.T) {
	e := newEvaluator(t, &stubProber{result: ProbeResult{Output: probeTranscript}})
	req := Request{
		Repo: "acme/web", BaseSHA: "sha-old", Command: []string{"make", "ci"},
		FailureText: stageResultText, RunID: "run-1", Waiter: "101",
	}
	if _, err := e.Classify(context.Background(), req); err != nil {
		t.Fatalf("Classify: %v", err)
	}

	if released, err := e.ReleaseReady("acme/web", "sha-old"); err != nil || len(released) != 0 {
		t.Fatalf("ReleaseReady on the same base = %v, %v; want nothing released while the base is unchanged", released, err)
	}

	released, err := e.ReleaseReady("acme/web", "sha-new")
	if err != nil {
		t.Fatalf("ReleaseReady: %v", err)
	}
	if len(released) != 1 || released[0].Subject != "101" {
		t.Fatalf("released = %+v, want subject 101 once the base advanced", released)
	}
	for _, blocker := range e.Store.Blockers("acme/web") {
		if len(blocker.Waiting) != 0 {
			t.Fatalf("blocker %s still holds %+v, want the released subject dropped", blocker.Key, blocker.Waiting)
		}
	}
}
