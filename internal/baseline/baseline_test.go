package baseline

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

type stubProber struct {
	result ProbeResult
	err    error
	calls  int
}

func (p *stubProber) Probe(context.Context, ProbeTarget, []string) (ProbeResult, error) {
	p.calls++
	return p.result, p.err
}

func newEvaluator(t *testing.T, prober Prober) *Evaluator {
	t.Helper()
	store, err := OpenStore(filepath.Join(t.TempDir(), "baseline.json"))
	if err != nil {
		t.Fatalf("OpenStore: %v", err)
	}
	at := time.Date(2026, 8, 30, 7, 0, 0, 0, time.UTC)
	return &Evaluator{Store: store, Prober: prober, Now: func() time.Time { return at }}
}

const failureText = "--- FAIL: TestAgentInstructions (0.01s)\n    agent-instructions-validation.test.ts:42: expected 3 sections, got 2"

func TestClassifyBlamesTheBranchWhenTheBaselineIsGreen(t *testing.T) {
	prober := &stubProber{result: ProbeResult{Green: true}}
	e := newEvaluator(t, prober)

	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: failureText, RunID: "run-1", Waiter: "101",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassPRIntroduced {
		t.Fatalf("class = %q, want %q", decision.Class, ClassPRIntroduced)
	}
	if decision.Park {
		t.Fatal("a green baseline must never park the branch under test")
	}
	if got := e.Store.Blockers("acme/web"); len(got) != 0 {
		t.Fatalf("blockers = %d, want 0 for a branch-introduced failure", len(got))
	}
}

func TestClassifyDetectsAnIdenticalBaseFailure(t *testing.T) {
	prober := &stubProber{result: ProbeResult{Output: failureText}}
	e := newEvaluator(t, prober)
	req := Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: failureText, RunID: "run-1", Waiter: "101",
	}

	decision, err := e.Classify(context.Background(), req)
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassSharedBaselineFailure {
		t.Fatalf("class = %q, want %q", decision.Class, ClassSharedBaselineFailure)
	}
	if !decision.Park {
		t.Fatal("park = false, want true: the shared repair lane is off by default")
	}
	if decision.BlockerKey == "" || decision.Waiting != 1 {
		t.Fatalf("blocker = %q waiting = %d, want one durable blocker with one waiter", decision.BlockerKey, decision.Waiting)
	}

	// A second, unrelated branch hitting the same red base must reuse both the
	// cached observation (no second probe) and the same durable blocker.
	second := req
	second.RunID, second.Waiter = "run-2", "202"
	decision2, err := e.Classify(context.Background(), second)
	if err != nil {
		t.Fatalf("Classify second: %v", err)
	}
	if prober.calls != 1 {
		t.Fatalf("probes = %d, want 1: only the first affected run pays to measure the baseline", prober.calls)
	}
	if decision2.BlockerKey != decision.BlockerKey {
		t.Fatalf("blocker = %q, want the shared %q", decision2.BlockerKey, decision.BlockerKey)
	}
	if decision2.Waiting != 2 {
		t.Fatalf("waiting = %d, want 2 parked subjects on one blocker", decision2.Waiting)
	}
}

func TestClassifyBlamesTheBranchOnADifferentSignature(t *testing.T) {
	prober := &stubProber{result: ProbeResult{Output: "--- FAIL: TestSomethingElse\n    other_test.go:9: boom"}}
	e := newEvaluator(t, prober)

	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: failureText,
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassPRIntroduced {
		t.Fatalf("class = %q, want %q: a red base with a different failure does not excuse this one", decision.Class, ClassPRIntroduced)
	}
}

func TestClassifyWithoutEvidenceIsUnknown(t *testing.T) {
	e := newEvaluator(t, nil)

	noBase, err := e.Classify(context.Background(), Request{Repo: "acme/web", Command: []string{"make", "ci"}, FailureText: failureText})
	if err != nil {
		t.Fatalf("Classify without base SHA: %v", err)
	}
	if noBase.Class != ClassUnknown {
		t.Fatalf("class = %q, want %q without a pinned base SHA", noBase.Class, ClassUnknown)
	}

	noProber, err := e.Classify(context.Background(), Request{Repo: "acme/web", BaseSHA: "abc", Command: []string{"make", "ci"}, FailureText: failureText})
	if err != nil {
		t.Fatalf("Classify without prober: %v", err)
	}
	if noProber.Class != ClassUnknown {
		t.Fatalf("class = %q, want %q with no cached baseline and no prober", noProber.Class, ClassUnknown)
	}
}

func TestClassifyReportsProbeFailures(t *testing.T) {
	e := newEvaluator(t, &stubProber{err: errors.New("worktree unavailable")})

	if _, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123", Command: []string{"make", "ci"}, FailureText: failureText,
	}); err == nil {
		t.Fatal("Classify error = nil, want the probe failure surfaced to the caller")
	}
}

func TestRepairLaneKeepsTheBranchRunning(t *testing.T) {
	e := newEvaluator(t, &stubProber{result: ProbeResult{Output: failureText}})
	e.RepairLane = true

	decision, err := e.Classify(context.Background(), Request{
		Repo: "acme/web", BaseSHA: "abc123def456", Command: []string{"make", "ci"},
		FailureText: failureText, Waiter: "101",
	})
	if err != nil {
		t.Fatalf("Classify: %v", err)
	}
	if decision.Class != ClassSharedBaselineFailure {
		t.Fatalf("class = %q, want the failure still classified as shared", decision.Class)
	}
	if decision.Park {
		t.Fatal("park = true, want false when the shared repair lane is explicitly enabled")
	}
}

func TestEvaluatorRequiresAStore(t *testing.T) {
	var e *Evaluator
	if _, err := e.Classify(context.Background(), Request{}); !errors.Is(err, ErrNoStore) {
		t.Fatalf("err = %v, want ErrNoStore", err)
	}
}
