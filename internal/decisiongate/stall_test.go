package decisiongate

import (
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
)

func stallGate(t *testing.T, f *fake) *Gate {
	t.Helper()
	g, err := New(f, Config{Thresholds: map[string]Threshold{StallThresholdKey: DefaultStallThreshold}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func progressWindow(n int) string {
	var b strings.Builder
	for i := range 10 {
		fmt.Fprintf(&b, "step %c%c output line\n", 'a'+byte(n%20), 'a'+byte(i))
	}
	return b.String()
}

func TestStallFacts(t *testing.T) {
	loop := strings.Repeat("run go test ./... (took 3s)\nFAIL pkg 12\n", 5)
	f := StallFactsOf(loop)
	if f.LoopCycles != 5 || f.FileChanged {
		t.Fatalf("loop facts = %+v", f)
	}
	if v, ok := DecideStallByCode(f); !ok || v != StallDetected {
		t.Fatalf("loop without file change = %q, %v", v, ok)
	}
	f = StallFactsOf(loop + "apply_patch internal/x.go\n")
	if _, ok := DecideStallByCode(StallFactsOf(strings.Repeat("go test\n", 6) + "wrote file a.go\n" + strings.Repeat("go test\n", 5))); ok {
		t.Fatalf("file change inside window must not be a code stall; facts=%+v", f)
	}
	if f := StallFactsOf(progressWindow(1)); f.LoopCycles >= repeatCycles {
		t.Fatalf("progressing window looks like a loop: %+v", f)
	}
}

func TestEvaluateStallCodeFactsSkipModel(t *testing.T) {
	f := &fake{yes: 0.01}
	g := stallGate(t, f)
	v, basis, _, err := g.EvaluateStall(t.Context(), strings.Repeat("same call\n", 12))
	if err != nil || v != StallDetected || basis != StallBasisCode {
		t.Fatalf("got %q %q %v", v, basis, err)
	}
	v, basis, _, _ = g.EvaluateStall(t.Context(), "one\ntwo\n")
	if v != StallUnsure || basis != StallBasisNone || f.calls.Load() != 0 {
		t.Fatalf("short window = %q %q calls=%d", v, basis, f.calls.Load())
	}
}

func TestEvaluateStallModelThresholds(t *testing.T) {
	for name, tc := range map[string]struct {
		yes  float64
		want StallVerdict
	}{"stalled": {0.95, StallDetected}, "progressing": {0.05, StallProgressing}, "unsure": {0.5, StallUnsure}} {
		g := stallGate(t, &fake{yes: tc.yes})
		if v, basis, _, err := g.EvaluateStall(t.Context(), progressWindow(1)); err != nil || v != tc.want || basis != StallBasisModel {
			t.Fatalf("%s: got %q %q %v", name, v, basis, err)
		}
	}
	g := stallGate(t, &fake{err: fmt.Errorf("down")})
	if v, _, _, err := g.EvaluateStall(t.Context(), progressWindow(1)); err == nil || v != StallUnsure {
		t.Fatalf("error must be unsure, got %q %v", v, err)
	}
}

func TestStallObserverBurstKeepsLatestWindow(t *testing.T) {
	f := &fake{yes: 0.99, delay: 40 * time.Millisecond}
	var mu sync.Mutex
	var records []StallRecord
	var outcomes []StallOutcome
	o := NewStallObserver(stallGate(t, f), 1, 1, func(r StallRecord) { mu.Lock(); records = append(records, r); mu.Unlock() },
		func(out StallOutcome) { mu.Lock(); outcomes = append(outcomes, out); mu.Unlock() })
	const total = 6
	for i := 1; i <= total; i++ {
		o.ObserveWindow("run|stage", progressWindow(i))
	}
	o.Wait()
	mu.Lock()
	if len(records) == 0 || records[len(records)-1].Window != total {
		t.Fatalf("latest burst window was not scored: %+v", records)
	}
	if len(records) >= total {
		t.Fatalf("expected coalescing, scored %d of %d", len(records), total)
	}
	mu.Unlock()
	o.FinishStream("run|stage", true)
	o.FinishStream("run|stage", true)
	mu.Lock()
	defer mu.Unlock()
	if len(outcomes) != 1 || outcomes[0].FlaggedWindow == 0 || !outcomes[0].BudgetHit || outcomes[0].Windows != total {
		t.Fatalf("outcomes = %+v", outcomes)
	}
}

func TestStallObserverLoopNeedsNoModelAndTally(t *testing.T) {
	f := &fake{yes: 0.01}
	var flagged []StallRecord
	var outcome StallOutcome
	o := NewStallObserver(stallGate(t, f), 1, 1, func(r StallRecord) { flagged = append(flagged, r) }, func(out StallOutcome) { outcome = out })
	o.ObserveWindow("s", progressWindow(1))
	o.Wait()
	o.ObserveWindow("s", strings.Repeat("retry fetch\n", 10))
	o.Wait()
	o.FinishStream("s", false)
	if f.calls.Load() != 1 || len(flagged) != 2 || flagged[0].Verdict != StallProgressing || flagged[1].Basis != StallBasisCode || outcome.FlaggedWindow != 2 {
		t.Fatalf("calls=%d flagged=%+v outcome=%+v", f.calls.Load(), flagged, outcome)
	}

	var tally StallTally
	tally.Add(StallOutcome{FlaggedWindow: 2, BudgetHit: true})
	tally.Add(outcome)
	tally.Add(StallOutcome{BudgetHit: true})
	tally.Add(StallOutcome{})
	if tally.FlaggedBudgetHit != 1 || tally.FlaggedCompleted != 1 || tally.MissedBudgetHit != 1 || tally.Streams != 4 {
		t.Fatalf("tally = %+v", tally)
	}
	if _, ok := tally.Precision(3); ok {
		t.Fatal("precision below the minimum sample must be refused")
	}
	if p, ok := tally.Precision(2); !ok || p != 0.5 {
		t.Fatalf("precision = %v %v", p, ok)
	}
}
