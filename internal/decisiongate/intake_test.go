package decisiongate

import (
	"context"
	"errors"
	"testing"
)

func intakeGate(t *testing.T, f *choiceFake) *Gate {
	t.Helper()
	g, err := New(f, Config{Thresholds: map[string]Threshold{IntakeThresholdKey: DefaultIntakeThreshold}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func TestExplicitDependencies(t *testing.T) {
	got := ExplicitDependencies("Blocked by #12 and depends on #7, also blocked by #12. See #99.")
	if len(got) != 2 || got[0] != "12" || got[1] != "7" {
		t.Fatalf("got %v", got)
	}
}

func TestIntakeCodeDecidesWithoutModel(t *testing.T) {
	f := &choiceFake{choice: "none", prob: 1}
	g := intakeGate(t, f)
	open := []IntakeItem{{ID: "12", Title: "Parser rewrite"}, {ID: "20", Title: "Cache eviction bug"}}

	v, err := g.EvaluateIntake(context.Background(), IntakeItem{ID: "30", Title: "x", Body: "Blocked by #12"}, open)
	if err != nil || v.Kind != IntakeDependency || v.Related != "12" || v.Basis != IntakeBasisCode {
		t.Fatalf("dependency: %+v %v", v, err)
	}
	v, _ = g.EvaluateIntake(context.Background(), IntakeItem{ID: "31", Title: "cache eviction BUG!"}, open)
	if v.Kind != IntakeDuplicate || v.Related != "20" || v.Basis != IntakeBasisCode {
		t.Fatalf("duplicate: %+v", v)
	}
	v, _ = g.EvaluateIntake(context.Background(), IntakeItem{ID: "32", Title: "Totally unrelated widget"}, open)
	if v.Kind != IntakeNone || v.Basis != IntakeBasisCode {
		t.Fatalf("none: %+v", v)
	}
	if f.calls.Load() != 0 {
		t.Fatalf("model called %d times for code-decidable intakes", f.calls.Load())
	}
}

func TestIntakeModelJudgesProse(t *testing.T) {
	open := []IntakeItem{{ID: "5", Title: "Agent transcript stall loop detection"}, {ID: "6", Title: "Unrelated billing page"}}
	item := IntakeItem{ID: "9", Title: "Agent transcript stall loop checks"}

	v, err := intakeGate(t, &choiceFake{choice: "dup-5", prob: 0.95}).EvaluateIntake(context.Background(), item, open)
	if err != nil || v.Kind != IntakeDuplicate || v.Related != "5" || v.Basis != IntakeBasisModel {
		t.Fatalf("got %+v %v", v, err)
	}
	v, _ = intakeGate(t, &choiceFake{choice: "dep-5", prob: 0.95}).EvaluateIntake(context.Background(), item, open)
	if v.Kind != IntakeDependency || v.Related != "5" {
		t.Fatalf("got %+v", v)
	}
	v, _ = intakeGate(t, &choiceFake{choice: "dup-5", prob: 0.5}).EvaluateIntake(context.Background(), item, open)
	if v.Kind != IntakeUnsure {
		t.Fatalf("low confidence must be unsure: %+v", v)
	}
	v, err = intakeGate(t, &choiceFake{err: errors.New("down")}).EvaluateIntake(context.Background(), item, open)
	if err == nil || v.Kind != IntakeUnsure {
		t.Fatalf("error must be unsure: %+v %v", v, err)
	}
}

func TestRankIntakeCandidatesBounds(t *testing.T) {
	var open []IntakeItem
	for i := range 400 {
		open = append(open, IntakeItem{ID: string(rune('a'+i%26)) + "x", Title: "agent transcript stall detection", Body: string(make([]byte, 5000))})
	}
	got := RankIntakeCandidates(IntakeItem{ID: "n", Title: "agent transcript stall"}, open)
	if len(got) != MaxIntakeCandidates {
		t.Fatalf("candidates = %d", len(got))
	}
	for _, c := range got {
		if len(c.Body) > maxIntakeBodyBytes {
			t.Fatalf("body not bounded: %d", len(c.Body))
		}
	}
	if RankIntakeCandidates(IntakeItem{ID: "1", Title: "the and"}, open) != nil {
		t.Fatal("stopword-only title must yield no candidates")
	}
}

func TestIntakeTallyRatesSuppressThinSample(t *testing.T) {
	var tl IntakeTally
	for range 5 {
		tl.Add(true, true)
		tl.Add(false, false)
	}
	if _, _, ok := tl.Rates(10); ok {
		t.Fatal("thin sample reported")
	}
	f, u, ok := tl.Rates(5)
	if !ok || f != 1 || u != 0 {
		t.Fatalf("rates %v %v %v", f, u, ok)
	}
}
