package decisioneval

import (
	"context"
	"os"
	"strings"
	"testing"
)

func runFixture(t *testing.T, keepReport int) Report {
	t.Helper()
	f, err := LoadFixture("testdata/claims_bad_input.json")
	if err != nil {
		t.Fatal(err)
	}
	if keepReport > 0 {
		var kept []Item
		n := 0
		for _, it := range f.Items {
			if it.Split == SplitReport {
				if n++; n > keepReport {
					continue
				}
			}
			kept = append(kept, it)
		}
		f.Items = kept
	}
	r, err := Run(context.Background(), f, NewReplayDecider(f.Items), NewReplayJudge(f.Items), Options{Repeats: 3})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func TestRunScoresBothArms(t *testing.T) {
	r := runFixture(t, 0)
	if r.Threshold.Accept != 0.70 {
		t.Fatalf("threshold chosen on tune split = %v, want 0.70", r.Threshold.Accept)
	}
	s, j := r.SystemOne, r.Judge
	if s.TP != 21 || s.FP != 0 || s.FalsePass() != 1 || s.Unsure != 2 || s.Negative != 22 {
		t.Fatalf("system one counts wrong: %+v", s)
	}
	if j.TP != 19 || j.FalseBlock() != 2 || j.FalsePass() != 3 {
		t.Fatalf("judge counts wrong: %+v", j)
	}
	if s.Stable != s.N || r.Agreement.N != 44 {
		t.Fatalf("determinism/agreement wrong: %+v %+v", s, r.Agreement)
	}
	if ok, why := r.Graduation(); !ok {
		t.Fatalf("expected ready, got %s", why)
	}
}

func TestSmallSampleRefusesPercentages(t *testing.T) {
	r := runFixture(t, 5)
	out := r.Render()
	if !strings.Contains(out, "recall=n/a (n=5<20)") || strings.Contains(out, "recall=9") {
		t.Fatalf("percentages printed for small sample:\n%s", out)
	}
	if ok, _ := r.Graduation(); ok {
		t.Fatal("must not graduate on a small sample")
	}
}

func TestReportGolden(t *testing.T) {
	got := runFixture(t, 0).Render()
	const path = "testdata/report.golden"
	if os.Getenv("UPDATE_GOLDEN") == "1" {
		if err := os.WriteFile(path, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(want) != got {
		t.Fatalf("report differs from golden:\n%s", got)
	}
}

func TestLiveIsOptIn(t *testing.T) {
	if LiveEnabled(func(string) string { return "" }) {
		t.Fatal("live must be off by default")
	}
	if !LiveEnabled(func(k string) string {
		if k == LiveEnv {
			return "1"
		}
		return ""
	}) {
		t.Fatal("live must honor the opt-in variable")
	}
}
